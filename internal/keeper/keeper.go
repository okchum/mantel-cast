// Package keeper keeps the dashboard on a screen (a Nest Hub, a kiosk
// tablet): it watches what the device shows and puts the dashboard back
// whenever the device falls back to its idle (ambient, photo frame, screen
// off) state, without stealing the screen from other apps. The screen
// technology is a display.Driver; the keeper only decides when.
package keeper

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/okchum/mantel-cast/internal/cast"
	"github.com/okchum/mantel-cast/internal/config"
	"github.com/okchum/mantel-cast/internal/display"
	"github.com/okchum/mantel-cast/internal/fully"
	"github.com/okchum/mantel-cast/internal/ha"
)

type Keeper struct {
	Cfg config.Cast
	// Driver is the screen technology; nil = NewDriver(Cfg).
	Driver display.Driver
	// URL is the page the device opens. A host of "auto" (http://auto:8080/)
	// stands for this machine's LAN address as seen from the device, worked
	// out again before every launch so a DHCP change heals by itself.
	URL string
	Loc *time.Location // nil = time.Local
	// Reachable checks the page (with its token) answers before casting it;
	// nil means an HTTP GET from this machine (tests replace it).
	Reachable func(ctx context.Context, url string) error
	Now       func() time.Time // nil = time.Now
	// Phone returns the latest phone sleep report (nil, nil = none yet).
	// Nil func: no phone signal.
	Phone func(ctx context.Context) (*Asleep, error)
	// NightMode reads the device's night mode; nil = the session's own
	// (display.NightModer), none if the driver has no night mode.
	NightMode func(ctx context.Context, host string) (*display.NightMode, error)

	// Seams for tests: nil = the driver's Dial and Discover, a real sleep.
	dial     func(ctx context.Context, host string, port int) (display.Session, error)
	discover func(ctx context.Context, timeout time.Duration) ([]display.Device, error)
	sleep    func(ctx context.Context, d time.Duration) bool

	sess       display.Session // the live session, while runSession runs
	host       string          // last device address that answered
	port       int
	deviceName string
	deviceID   string // its identity, when discovery or the device told it
	checked    bool   // a status has been seen since this process started
	session    string // session id of the app this process launched (or adopted)
	loadedOnce bool
	lastMsg    string
	shown      string // URL last loaded

	// crash-loop guard: an app that keeps closing right after launch (page
	// stuck on "Force loading…") is not relaunched forever.
	relaunches []time.Time
	upSince    time.Time // when this process last cast the page
	seenUp     time.Time // last check that found it still showing
	pauseUntil time.Time
	pauseStep  time.Duration

	hub              *display.NightMode // last good night mode reading
	hubAt            time.Time
	hubErr, phoneErr string // last logged errors (log on change only)
}

var logf = log.Printf

// httpClient talks to Mantel (the page probe, phone sleep reports). It
// ignores HTTP(S)_PROXY: Mantel is usually on the LAN, and the probe must
// see what the device will see. Requests are bounded by their ctx.
var httpClient = func() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	return &http.Client{Transport: t}
}()

// probeClient is httpClient without following redirects: the probe's
// answer is the page's first status (Mantel sets its cookie on the ?k=
// request; following a redirect without a cookie jar would 401).
var probeClient = &http.Client{
	Transport:     httpClient.Transport,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// NewDriver is the screen technology cfg.Driver names.
func NewDriver(cfg config.Cast) (display.Driver, error) {
	switch cfg.Driver {
	case "", config.DriverCast:
		return cast.Driver{Options: cast.Options{AppID: cfg.App(), Reload: cfg.Reload}}, nil
	case config.DriverFully:
		return fully.Driver{Password: cfg.Password}, nil
	case config.DriverHA:
		if cfg.HA == nil {
			return nil, fmt.Errorf("the %q driver needs cast.ha", config.DriverHA)
		}
		return ha.Driver{URL: cfg.HA.URL, Token: cfg.HA.Token, Path: cfg.HA.Path, Home: cfg.HA.Home}, nil
	}
	return nil, fmt.Errorf("unknown cast.driver %q", cfg.Driver)
}

func (k *Keeper) driver() display.Driver {
	if k.Driver != nil {
		return k.Driver
	}
	if d, err := NewDriver(k.Cfg); err == nil {
		return d
	}
	// Normalize rejects unknown drivers; Cast is the fallback.
	return cast.Driver{Options: cast.Options{AppID: k.Cfg.App(), Reload: k.Cfg.Reload}}
}

// Reconnect backoff: doubles from backoffMin to backoffMax, back to
// backoffMin after a session that stayed up for healthyAfter. The cap is
// short because the usual cause is the LAN itself going away for a few
// minutes (a router reboot): the dashboard should be back within half a
// minute of the network, and a dial or mDNS query that often costs nothing.
const (
	backoffMin   = 5 * time.Second
	backoffMax   = 30 * time.Second
	healthyAfter = time.Minute
)

func (k *Keeper) report(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if k.lastMsg != msg {
		logf("cast: %s", msg)
	}
	k.lastMsg = msg
}

func (k *Keeper) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

func (k *Keeper) loc() *time.Location {
	if k.Loc != nil {
		return k.Loc
	}
	return time.Local
}

// Run blocks until ctx is cancelled, reconnecting with backoff.
func (k *Keeper) Run(ctx context.Context) {
	sleep := k.sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	var b backoff
	for ctx.Err() == nil {
		start := k.now()
		err := k.runSession(ctx)
		if ctx.Err() != nil {
			return
		}
		k.report("error: %v", err)
		// The next attempt dials the last address that answered first and
		// only falls back to discovery when that fails (see connect).
		if !sleep(ctx, b.next(k.now().Sub(start))) {
			return
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// backoff is the wait before reconnecting.
type backoff struct{ cur time.Duration }

// next is the wait after a session that lasted this long: a healthy session
// starts over from backoffMin, a quick failure doubles the previous wait.
func (b *backoff) next(lasted time.Duration) time.Duration {
	if b.cur == 0 || lasted >= healthyAfter {
		b.cur = backoffMin
	} else {
		b.cur = min(b.cur*2, backoffMax)
	}
	return b.cur
}

// connect dials the device: the last address that answered first (if the
// same device still answers there), then the configured host or discovery.
func (k *Keeper) connect(ctx context.Context) (display.Session, error) {
	dial := k.dial
	if dial == nil {
		dial = k.driver().Dial
	}
	if k.Cfg.Host == "" && k.host != "" {
		s, err := dial(ctx, k.host, k.port)
		switch {
		case err != nil:
			logf("cast: %s at %s does not answer (%v), looking for it again", k.deviceName, k.host, err)
		case k.sameDevice(ctx, s):
			return s, nil
		default:
			_ = s.Close()
		}
	}
	d, err := resolve(ctx, k.Cfg, k.discover, k.driver())
	if err != nil {
		return nil, err
	}
	s, err := dial(ctx, d.Host, d.Port)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", d.Host, err)
	}
	k.host, k.port, k.deviceName, k.deviceID = d.Host, d.Port, d.Name, d.ID
	return s, nil
}

// sameDevice reports whether the device answering at the remembered
// address is still the one found before: DHCP may have given the address to
// another device of the same kind. Sessions that cannot tell, or a failed
// read, count as the same.
func (k *Keeper) sameDevice(ctx context.Context, s display.Session) bool {
	id, ok := s.(display.Identifier)
	if !ok {
		return true
	}
	ictx, cancel := context.WithTimeout(ctx, k.Cfg.Timings().CommandTimeout)
	d, err := id.Identify(ictx)
	cancel()
	if err != nil {
		return true
	}
	differs := false
	if k.deviceID != "" && d.ID != "" {
		differs = normID(d.ID) != normID(k.deviceID)
	} else if k.deviceName != "" && d.Name != "" {
		differs = !strings.EqualFold(d.Name, k.deviceName)
	}
	if differs {
		logf("cast: %s now answers as %q, not %s; looking for %s again", k.host, d.Name, k.deviceName, k.deviceName)
	}
	return !differs
}

func normID(s string) string { return strings.ReplaceAll(strings.ToLower(s), "-", "") }

// Resolve finds the target device: the configured host, or the driver's
// discovery matched by name.
func Resolve(ctx context.Context, cfg config.Cast, drv display.Driver) (display.Device, error) {
	return resolve(ctx, cfg, nil, drv)
}

func resolve(ctx context.Context, cfg config.Cast, discover func(context.Context, time.Duration) ([]display.Device, error), drv display.Driver) (display.Device, error) {
	if cfg.Host != "" {
		return display.Device{Name: cfg.Host, Host: cfg.Host, Port: cfg.Port}, nil
	}
	if discover == nil {
		discover = drv.Discover
	}
	devs, err := discover(ctx, cfg.Timings().DiscoverTimeout)
	if err != nil {
		return display.Device{}, fmt.Errorf("discover: %w", err)
	}
	return pickDevice(devs, cfg.Device)
}

// pickDevice matches want (case-insensitively) against the devices: an exact
// name first, then the UUID, then a name or model substring. Several
// matches at the same level take the first and log a warning.
func pickDevice(devs []display.Device, want string) (display.Device, error) {
	want = strings.ToLower(strings.TrimSpace(want))
	tiers := []func(d display.Device) bool{
		func(d display.Device) bool { return want == "" },
		func(d display.Device) bool { return strings.ToLower(d.Name) == want },
		func(d display.Device) bool { return d.ID != "" && normID(d.ID) == normID(want) },
		func(d display.Device) bool {
			return strings.Contains(strings.ToLower(d.Name), want) || strings.Contains(strings.ToLower(d.Model), want)
		},
	}
	if want != "" {
		tiers = tiers[1:]
	}
	for _, match := range tiers {
		var found []display.Device
		for _, d := range devs {
			if match(d) {
				found = append(found, d)
			}
		}
		if len(found) == 0 {
			continue
		}
		if len(found) > 1 {
			logf("cast: %d devices match %q (%s), using %q; set cast.device to its full name or UUID, or cast.host",
				len(found), want, deviceNames(found), found[0].Name)
		}
		return found[0], nil
	}
	return display.Device{}, fmt.Errorf("no device matching %q (found: %s)", want, deviceNames(devs))
}

func deviceNames(devs []display.Device) string {
	names := make([]string, 0, len(devs))
	for _, d := range devs {
		names = append(names, fmt.Sprintf("%q", d.Name))
	}
	return strings.Join(names, ", ")
}

func (k *Keeper) runSession(ctx context.Context) error {
	c, err := k.connect(ctx)
	if err != nil {
		return err
	}
	k.sess = c
	defer func() { k.sess = nil; _ = c.Close() }()

	tick := time.NewTicker(k.Cfg.Timings().CheckInterval)
	defer tick.Stop()
	for {
		// check only returns connection-level errors; the device refusing a
		// command is reported and retried on the next tick.
		if err := k.check(ctx, c); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-c.Done():
			return errors.New("connection to device closed")
		case <-tick.C:
		}
	}
}

// appLevel reports a device refusing a command (it answered, the
// connection is fine) and swallows it; anything else is returned so the
// connection is rebuilt.
func (k *Keeper) appLevel(what string, err error) error {
	if errors.Is(err, display.ErrRefused) {
		k.report("%s: %v", what, err)
		return nil
	}
	return fmt.Errorf("%s: %w", what, err)
}

// check runs one round: see what the device shows, then close the dashboard
// for the quiet hours, leave it, or (re)cast it.
func (k *Keeper) check(ctx context.Context, c display.Session) error {
	t := k.Cfg.Timings()
	now := k.now()
	// The quiet signals come over the network (Mantel, the device's setup
	// API); read them before the device deadlines start so a slow fetch
	// cannot starve the status or stop request.
	quiet, why := k.quietNow(ctx, now)

	scr, err := k.observe(ctx, c, t)
	if err != nil {
		return err
	}
	ourApp, ours, idle := scr.Ours, scr.ours, scr.Idle
	if quiet {
		return k.closeForQuiet(ctx, c, t, scr, why)
	}

	loadsPage := k.driver().LoadsPage()
	want := k.URL
	if loadsPage {
		if want, err = k.target(); err != nil {
			k.report("not casting: %v", err)
			return nil
		}
	}
	switch {
	case ours && k.loadedOnce && (!loadsPage || want == k.shown):
		k.seenUp = now
		if !k.upSince.IsZero() && now.Sub(k.upSince) >= t.StableAfter {
			k.pauseStep = 0
		}
		k.report("showing")
		return nil
	case ours && k.loadedOnce:
		logf("cast: page address changed (%s → %s), reloading", Redact(k.shown), Redact(want))
	case ours:
		// Our receiver is up but this process never loaded it (e.g. we restarted
		// and the page may be a stale error screen) — load once more.
	case !idle && !k.Cfg.TakeOver:
		// Someone chose to watch something else (or cast DashCast themselves):
		// the dashboard leaving for that is not a crash.
		k.loadedOnce, k.relaunches = false, nil
		name := scr.Showing
		if ourApp {
			name += " (cast by someone else)"
		}
		k.report("busy: %s", name)
		return nil
	}
	if now.Before(k.pauseUntil) {
		k.report("paused until %s: the dashboard kept closing on the device", k.pauseUntil.In(k.loc()).Format("15:04"))
		return nil
	}
	// The page we cast is gone. Only an exit soon after launch counts
	// towards the crash-loop guard; seenUp-upSince is how long it was
	// known to be up.
	gone := k.loadedOnce && !ours
	if gone && idle && k.seenUp.Sub(k.upSince) < t.CrashIfUpUnder && k.crashLooping(now, t) {
		k.report("paused until %s: the dashboard kept closing on the device", k.pauseUntil.In(k.loc()).Format("15:04"))
		return nil
	}
	if loadsPage && !k.pageAnswers(ctx, want, t.ProbeTimeout) {
		return nil
	}
	lctx, cancel := context.WithTimeout(ctx, t.LaunchTimeout)
	defer cancel()
	sess, err := c.Show(lctx, want)
	if err != nil {
		return k.appLevel("cast", err)
	}
	if gone {
		// Logged every time: an app that keeps exiting right after launch
		// would otherwise look like a steady "showing".
		logf("cast: dashboard was gone, launched it again")
	}
	k.loadedOnce, k.shown, k.upSince, k.session = true, want, now, sess
	k.report("showing")
	return nil
}

// screen is what the device shows, as check sees it.
type screen struct {
	display.Screen      // Ours: the configured app or page, whoever started it
	ours           bool // ... in the session this process started (or adopted)
}

// observe reads what the device shows and works out whose it is.
func (k *Keeper) observe(ctx context.Context, c display.Session, t config.Timings) (screen, error) {
	sctx, cancel := context.WithTimeout(ctx, t.CommandTimeout)
	sc, err := c.Observe(sctx)
	cancel()
	if err != nil {
		return screen{}, fmt.Errorf("status: %w", err)
	}
	// Ownership: a session this process started is ours. Our app with any
	// other handle belongs to another sender, except at the first check
	// after Mantel Cast starts, when it is assumed to be ours from before the
	// restart and adopted (reloaded once, closed during quiet).
	if sc.Ours && !k.checked && k.session == "" {
		k.session = sc.Handle
	}
	k.checked = true
	return screen{Screen: sc, ours: sc.Ours && k.session != "" && sc.Handle == k.session}, nil
}

// closeForQuiet closes our dashboard for the quiet hours; other apps are left alone.
func (k *Keeper) closeForQuiet(ctx context.Context, c display.Session, t config.Timings, s screen, why string) error {
	if s.ours {
		// Hand the screen back so the device shows its own dimmed night
		// clock; the dashboard comes back when the quiet ends.
		stctx, cancel := context.WithTimeout(ctx, t.CommandTimeout)
		err := c.Hide(stctx, s.Handle)
		cancel()
		if err != nil {
			return k.appLevel("stop for quiet", err)
		}
		k.session = ""
		logf("cast: quiet (%s), closed the dashboard", why)
	}
	k.loadedOnce, k.relaunches = false, nil
	k.report("quiet (%s)", why)
	return nil
}

// crashLooping records a quick exit of the dashboard at now and reports
// whether that makes relaunch_limit inside relaunch_window, in which case
// re-casting is paused (pauseUntil, doubling from pause_min to pause_max).
func (k *Keeper) crashLooping(now time.Time, t config.Timings) bool {
	k.relaunches = append(k.relaunches, now)
	for len(k.relaunches) > 0 && now.Sub(k.relaunches[0]) > t.RelaunchWindow {
		k.relaunches = k.relaunches[1:]
	}
	if len(k.relaunches) < t.RelaunchLimit {
		return false
	}
	k.relaunches = nil
	k.pauseStep = min(max(k.pauseStep*2, t.PauseMin), t.PauseMax)
	k.pauseUntil = now.Add(k.pauseStep)
	return true
}

// pageAnswers probes the page before the device loads it and reports why
// not when it does not answer: DashCast shows "Force loading …" forever for
// a page it cannot reach, then the device gives up, and a kiosk browser shows
// its error page; don't put that on screen. The probe carries the token, so
// a rejected token is caught here too, not shown as Mantel's 401 page.
func (k *Keeper) pageAnswers(ctx context.Context, want string, timeout time.Duration) bool {
	check := k.Reachable
	if check == nil {
		check = reachable
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	err := check(rctx, want)
	cancel()
	var hs *statusError
	switch {
	case err == nil:
		return true
	case errors.As(err, &hs):
		k.report("not casting: %s: %v", Redact(want), err)
	case certErr(err):
		k.report("not casting: %s: bad certificate (%v); the device would refuse it too", Redact(want), err)
	default:
		k.report("not casting: %s does not answer (%v)", Redact(want), err)
	}
	return false
}

// Target is the page URL as it would be loaded now ("auto" resolved), or
// URL itself if that fails.
func (k *Keeper) Target() string {
	if u, err := k.target(); err == nil {
		return u
	}
	return k.URL
}

// target is the URL to load now, with an "auto" host resolved to the address
// of this machine on the route to the device.
func (k *Keeper) target() (string, error) { return ResolveAuto(k.URL, k.host) }

// ResolveAuto replaces a host of exactly "auto" (http://auto:8080/) with
// the address this machine uses to reach deviceHost; other URLs pass
// through unchanged. Errors never contain the URL's query (the token).
func ResolveAuto(u, deviceHost string) (string, error) {
	p, err := url.Parse(u)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", fmt.Errorf("dashboard URL %s: %w", Redact(u), err)
	}
	if p.Hostname() != "auto" {
		return u, nil
	}
	ip, err := LocalIP(deviceHost)
	if err != nil && deviceHost != "" {
		// A device known by a name that is not an address (a Home
		// Assistant browser ID): any LAN route will do.
		ip, err = LocalIP("")
	}
	if err != nil {
		return "", err
	}
	if port := p.Port(); port != "" {
		p.Host = net.JoinHostPort(ip, port)
	} else if strings.Contains(ip, ":") {
		p.Host = "[" + ip + "]"
	} else {
		p.Host = ip
	}
	return p.String(), nil
}

// LocalIP is the address this machine uses to reach target (no packet is
// sent: a UDP "connect" only picks the route).
func LocalIP(target string) (string, error) {
	if target == "" {
		target = "192.168.0.1" // any LAN-ish address picks the right interface
	}
	conn, err := net.Dial("udp", net.JoinHostPort(target, "9"))
	if err != nil {
		return "", fmt.Errorf("cannot work out this machine's LAN address: %w", err)
	}
	defer func() { _ = conn.Close() }()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

// reachable reports whether the page at u answers: any 2xx or 3xx (a
// redirect is not followed). 401/403 mean the token is missing or rejected
// and other statuses a page that is not there (a statusError either way):
// casting it would only show an error page. Certificates are verified like
// the device will: a page the Hub refuses is not worth casting.
func reachable(ctx context.Context, u string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return err
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 400 {
		return nil
	}
	return &statusError{code: resp.StatusCode, token: req.URL.Query().Get("k") != ""}
}

// statusError is the page answering with an error status.
type statusError struct {
	code  int
	token bool // the probed URL carried ?k=
}

func (e *statusError) Error() string {
	switch {
	case (e.code == http.StatusUnauthorized || e.code == http.StatusForbidden) && e.token:
		return fmt.Sprintf("Mantel rejected the token (HTTP %d); check \"token\" against Mantel's tokens", e.code)
	case e.code == http.StatusUnauthorized || e.code == http.StatusForbidden:
		return fmt.Sprintf("the page needs a token (HTTP %d); set \"token\" in mantel-cast.json", e.code)
	case e.code == http.StatusMisdirectedRequest:
		return fmt.Sprintf("Mantel refused this host name (HTTP %d); add it to Mantel's allowed_hosts", e.code)
	}
	return fmt.Sprintf("the page answers HTTP %d", e.code)
}

// certErr reports whether err is a TLS certificate verification failure.
func certErr(err error) bool {
	var cv *tls.CertificateVerificationError
	return errors.As(err, &cv)
}
