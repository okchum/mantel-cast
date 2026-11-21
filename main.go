// Mantel Cast keeps a Google Nest Hub (or another Cast device, or a tablet
// running Fully Kiosk Browser) showing a Mantel dashboard. Cast only works
// inside the LAN, so Mantel Cast runs at home even when Mantel lives on a public
// server: it re-casts when the device drops back to its idle screen, stays
// out of the way while something else plays, and goes quiet at night.
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/okchum/mantel-cast/internal/config"
	"github.com/okchum/mantel-cast/internal/display"
	"github.com/okchum/mantel-cast/internal/keeper"
	"github.com/okchum/mantel-kit/conf"
)

var version = "dev"

const usage = `mantel-cast %s — keeps a Google Nest Hub (or a Fully Kiosk tablet) on your Mantel dashboard

Usage:
  mantel-cast [run] [-config mantel-cast.json] [-mantel url] [-token t] [-device name] [-host ip] [-app id]
                                           keep the device on the dashboard (runs inside the LAN)
  mantel-cast discover [-config f]         list Cast devices on the LAN (cast driver)
  mantel-cast status [-config f] [-device name] [-host ip]
                                           show what the device is running
  mantel-cast cast   [-config f] [-device name] [-host ip] [url]
                                           cast once (default: the configured dashboard)
  mantel-cast stop   [-config f] [-device name] [-host ip]
                                           stop whatever app the device is running
                                           (fully: turn the screen off)

The config may be a mantel-cast.json ({"mantel", "token", "timezone", "cast"})
or Mantel's own config.json (public_url, tokens, timezone and cast are read).
Without -config, mantel-cast.json in the current directory is read.
`

const defaultConfig = "mantel-cast.json"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		// Back to the default handling once the first signal arrived, so a
		// second Ctrl-C kills a shutdown that hangs.
		<-ctx.Done()
		stop()
	}()

	var err error
	switch cmd {
	case "run", "keep":
		err = run(ctx, args)
	case "discover":
		err = discover(ctx, args)
	case "status", "cast", "stop":
		err = deviceCmd(ctx, cmd, args)
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Printf(usage, version)
	default:
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// Config is what Mantel Cast needs: where the dashboard is, how to get in, and
// which device to keep.
type Config struct {
	Mantel   string      `json:"mantel"` // dashboard page on Mantel, e.g. https://mantel.example.com/
	Token    string      `json:"token"`  // access token ("$MANTEL_TOKEN" reads the environment)
	Timezone string      `json:"timezone"`
	Cast     config.Cast `json:"cast"`

	loc *time.Location // Timezone resolved by loadConfig (time.Local when empty)
	// emptyEnv names the unset or empty variables the token read, for run's warning.
	emptyEnv []string
}

// mantelKeys mark a file as Mantel's config.json rather than a
// mantel-cast.json: Mantel's top-level keys that mantel-cast.json does not
// have (so never "mantel", "token", "timezone" or "cast").
var mantelKeys = []string{
	"public_url", "tokens", "listen", "pages", "servers", "sources", "title", "demo",
	"weather", "calendars", "probes", "quotas", "alerts", "actions", "display", "plans",
	"tls_cert", "tls_key", "interval", "state_file", "allowed_hosts",
}

// loadConfig reads path; a missing file gives defaults so flags alone work.
func loadConfig(path string) (*Config, error) {
	c := &Config{}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if c, err = parseConfig(b); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	// The token is expanded here, once, for both config kinds.
	c.emptyEnv = conf.EmptyEnv(c.Token)
	c.Token = conf.ExpandEnv(c.Token)
	c.Cast.Password = conf.ExpandEnv(c.Cast.Password)
	c.loc = time.Local
	if c.Timezone != "" {
		if c.loc, err = time.LoadLocation(c.Timezone); err != nil {
			return nil, fmt.Errorf("timezone: %w", err)
		}
	}
	return c, c.Cast.Normalize()
}

func parseConfig(b []byte) (*Config, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return nil, err
	}
	shared := ""
	for _, k := range mantelKeys {
		if _, ok := keys[k]; ok {
			shared = k
			break
		}
	}
	c := &Config{}
	if shared == "" {
		d := json.NewDecoder(bytes.NewReader(b))
		d.DisallowUnknownFields()
		if err := d.Decode(c); err != nil {
			return nil, fmt.Errorf("read as mantel-cast.json (no Mantel config keys such as public_url or tokens): %w", err)
		}
		return c, nil
	}
	// Mantel's config: take only what Mantel Cast needs and leave the rest to
	// Mantel. The cast section is Mantel Cast's own, so it is read strictly.
	var h struct {
		Listen    string          `json:"listen"`
		PublicURL string          `json:"public_url"`
		TLSCert   string          `json:"tls_cert"`
		Tokens    []config.Token  `json:"tokens"`
		Timezone  string          `json:"timezone"`
		Cast      json.RawMessage `json:"cast"`
	}
	mode := fmt.Sprintf("read as Mantel's config.json (it has %q)", shared)
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, fmt.Errorf("%s: %w", mode, err)
	}
	if len(h.Cast) > 0 && string(h.Cast) != "null" {
		d := json.NewDecoder(bytes.NewReader(h.Cast))
		d.DisallowUnknownFields()
		if err := d.Decode(&c.Cast); err != nil {
			return nil, fmt.Errorf("%s: cast: %w", mode, err)
		}
	}
	c.Mantel, c.Timezone = h.PublicURL, h.Timezone
	if c.Mantel == "" {
		// Mantel then serves the page at http://<LAN IP><listen>/: "auto"
		// is that LAN address, worked out by Mantel Cast.
		u, err := mantelPageURL(h.Listen, h.TLSCert != "")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", mode, err)
		}
		c.Mantel = u
	}
	c.Token = keeper.PickToken(h.Tokens) // expanded by loadConfig
	return c, nil
}

// mantelPageURL is the page address Mantel documents for an empty
// public_url: this machine's LAN address on the port of listen (Mantel's
// default ":8080"), over https when Mantel has a certificate.
func mantelPageURL(listen string, tls bool) (string, error) {
	if listen == "" {
		listen = ":8080"
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return "", fmt.Errorf("listen %q: want host:port or :port (or set public_url)", listen)
	}
	scheme := "http"
	if tls {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort("auto", port) + "/", nil
}

type deviceFlags struct {
	fs           *flag.FlagSet
	cfg          *string
	device, host *string
}

func addDeviceFlags(fs *flag.FlagSet) deviceFlags {
	return deviceFlags{
		fs:     fs,
		cfg:    fs.String("config", defaultConfig, "mantel-cast.json, or Mantel's config.json"),
		device: fs.String("device", "", "device name or model substring (overrides config)"),
		host:   fs.String("host", "", "device IP (skips discovery)"),
	}
}

func (f deviceFlags) load() (*Config, error) {
	c, err := loadConfig(*f.cfg)
	if err != nil {
		return nil, err
	}
	if *f.device != "" {
		c.Cast.Device = *f.device
	}
	if *f.host != "" {
		c.Cast.Host = *f.host
	}
	return c, nil
}

// runFlags are the flags of "mantel-cast run".
type runFlags struct {
	deviceFlags
	mantel, token, appID *string
}

func newRunFlags() runFlags {
	fs := flag.NewFlagSet("mantel-cast", flag.ExitOnError)
	return runFlags{
		deviceFlags: addDeviceFlags(fs),
		mantel:      fs.String("mantel", "", "dashboard URL (overrides config); host \"auto\" = this machine's LAN address, e.g. http://auto:8080/"),
		token:       fs.String("token", "", "access token (overrides config)"),
		appID:       fs.String("app", "", "Custom Receiver app id (overrides config)"),
	}
}

func run(ctx context.Context, args []string) error {
	f := newRunFlags()
	_ = f.fs.Parse(args)
	cfg, err := f.load()
	if err != nil {
		return err
	}
	if !cfg.Cast.IsEnabled() {
		// Idle rather than exit, so a service manager set to restart Mantel Cast
		// does not spin.
		log.Printf("mantel-cast: cast.enabled is false, not keeping any device (waiting for a signal)")
		<-ctx.Done()
		return nil
	}
	if *f.appID != "" {
		cfg.Cast.AppID = *f.appID
	}
	drv, err := keeper.NewDriver(cfg.Cast)
	if err != nil {
		return err
	}
	u := cmp.Or(*f.mantel, cfg.Mantel)
	tok := cmp.Or(*f.token, cfg.Token)
	// Only here: discover/status/stop do not use the token, and -token
	// replaces the configured one.
	if *f.token == "" {
		if w := tokenWarning(cfg.emptyEnv, tok, u, drv.LoadsPage()); w != "" {
			log.Printf("mantel-cast: warning: %s", w)
		}
	}
	if drv.LoadsPage() {
		if u == "" {
			return errors.New(`mantel-cast needs the dashboard URL: "mantel" in mantel-cast.json or -mantel https://… (or set cast.app_id to a Custom Receiver)`)
		}
		u = keeper.WithToken(u, tok)
		via := "DashCast"
		if cfg.Cast.Driver == config.DriverFully {
			via = "Fully Kiosk"
		}
		log.Printf("mantel-cast: %s with %s", via, keeper.Redact(u))
	} else {
		log.Printf("mantel-cast: Custom Receiver %s (its URL is set in the Cast console)", cfg.Cast.App())
	}
	k := &keeper.Keeper{Cfg: cfg.Cast, Driver: drv, URL: u, Loc: cfg.loc}
	if u != "" {
		// Phone sleep reports land on Mantel; ask it.
		k.Phone = func(ctx context.Context) (*keeper.Asleep, error) {
			return keeper.FetchAsleep(ctx, keeper.WithToken(k.Target(), tok))
		}
	}
	k.Run(ctx)
	return nil
}

// tokenWarning explains what a token that read unset or empty environment
// variables (emptyEnv) does to the run, or is "" when it changes nothing.
// tok is the token as expanded, page the dashboard URL, dashCast whether
// Mantel Cast loads that page itself (a Custom Receiver's page, with its own ?k=,
// is set in the Cast console; there the token only reads the phone sleep
// reports from Mantel).
func tokenWarning(emptyEnv []string, tok, page string, dashCast bool) string {
	if len(emptyEnv) == 0 || page == "" {
		return "" // no Mantel URL: the token is not used at all
	}
	if keeper.WithToken(page, "t") == page {
		return "" // the URL brings its own ?k=, so the token is not used
	}
	how := "without a token"
	if tok != "" {
		// "Bearer $A" with A unset: the rest of the token is still sent.
		how = "with a token missing that part"
	}
	which := "which is"
	if len(emptyEnv) > 1 {
		which = "which are"
	}
	what := "the page is opened " + how
	if !dashCast {
		what = "phone sleep reports are read from Mantel " + how
	}
	return fmt.Sprintf("the token reads $%s, %s unset or empty; %s",
		strings.Join(emptyEnv, ", $"), which, what)
}

func discover(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	path := fs.String("config", defaultConfig, "mantel-cast.json, or Mantel's config.json (for cast.discover_timeout)")
	_ = fs.Parse(args)
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	drv, err := keeper.NewDriver(cfg.Cast)
	if err != nil {
		return err
	}
	devs, err := drv.Discover(ctx, cfg.Cast.Timings().DiscoverTimeout)
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		fmt.Println("no Cast devices found (same LAN/VLAN? mDNS blocked?)")
	}
	for _, d := range devs {
		fmt.Printf("%-28s %-22s %s:%d  %s\n", d.Name, d.Model, d.Host, d.Port, d.ID)
	}
	return nil
}

func deviceCmd(ctx context.Context, cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	df := addDeviceFlags(fs)
	_ = fs.Parse(args)
	cfg, err := df.load()
	if err != nil {
		return err
	}
	drv, err := keeper.NewDriver(cfg.Cast)
	if err != nil {
		return err
	}
	d, err := keeper.Resolve(ctx, cfg.Cast, drv)
	if err != nil {
		return err
	}
	h, name := d.Host, d.Name
	c, err := drv.Dial(ctx, h, d.Port)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	t := cfg.Cast.Timings()
	timeout := t.CommandTimeout
	if cmd == "cast" {
		timeout = t.LaunchTimeout
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch cmd {
	case "status":
		sc, err := c.Observe(tctx)
		if err != nil {
			return err
		}
		fmt.Printf("%s (%s)\n", name, h)
		if len(sc.Detail) == 0 {
			fmt.Println("  no app running")
		}
		for _, l := range sc.Detail {
			fmt.Println("  " + l)
		}
		return nil
	case "stop":
		return stopScreen(tctx, c, name)
	default: // cast
		u := cmp.Or(fs.Arg(0), cfg.Mantel)
		if u == "" {
			return errors.New("nothing to cast: give a URL or set \"mantel\" in mantel-cast.json")
		}
		if u, err = keeper.ResolveAuto(u, h); err != nil {
			return err
		}
		u = keeper.WithToken(u, cfg.Token)
		fmt.Printf("casting %s to %s\n", keeper.Redact(u), name)
		_, err = c.Show(tctx, u)
		return err
	}
}

// stopScreen takes whatever shows off the device (unless it is already idle).
func stopScreen(ctx context.Context, c display.Session, name string) error {
	sc, err := c.Observe(ctx)
	if err != nil {
		return err
	}
	if sc.Idle {
		fmt.Println("nothing to stop")
		return nil
	}
	fmt.Printf("stopping %s on %s\n", sc.Showing, name)
	return c.Hide(ctx, sc.Handle)
}
