// Package keeper keeps the dashboard on the Nest Hub screen: it watches the
// receiver status and re-casts whenever the device falls back to its idle
// (ambient/photo frame) screen, without stealing the screen from other apps.
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
)

type Keeper struct {
	Cfg config.Cast
	// URL is the page DashCast opens. A host of "auto" (http://auto:8080/)
	// stands for this machine's LAN address as seen from the device, worked
	// out again before every launch so a DHCP change heals by itself.
	URL string
	Loc *time.Location // nil = time.Local
	// Reachable checks the page answers before casting it; nil means an HTTP
	// GET from this machine (tests replace it).
	Reachable func(ctx context.Context, url string) error
	Now       func() time.Time // nil = time.Now
	// Phone returns the latest phone sleep report (nil, nil = none yet).
	// Nil func: no phone signal.
	Phone func(ctx context.Context) (*Asleep, error)
	// NightMode reads the device's night mode; nil = cast.FetchNightMode.
	NightMode func(ctx context.Context, host string) (*cast.NightMode, error)

	// Seams for tests: nil = cast.Dial, cast.Discover, a real sleep.
	dial     func(ctx context.Context, host string, port int) (Conn, error)
	discover func(ctx context.Context, timeout time.Duration) ([]cast.Device, error)
	sleep    func(ctx context.Context, d time.Duration) bool

	host       string // last device address that answered
	port       int
	deviceName string
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

	hub              *cast.NightMode // last good night mode reading
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

// Conn is a live connection to the device.
type Conn interface {
	Device
	Done() <-chan struct{}
	Close() error
}

func dialCast(ctx context.Context, host string, port int) (Conn, error) {
	return cast.Dial(ctx, host, port)
}

// connect dials the device: the last address that answered first, then
// the configured host or mDNS discovery.
func (k *Keeper) connect(ctx context.Context) (Conn, error) {
	dial := k.dial
	if dial == nil {
		dial = dialCast
	}
	if k.Cfg.Host == "" && k.host != "" {
		c, err := dial(ctx, k.host, k.port)
		if err == nil {
			return c, nil
		}
		logf("cast: %s at %s does not answer (%v), looking for it again", k.deviceName, k.host, err)
	}
	host, port, name, err := resolve(ctx, k.Cfg, k.discover)
	if err != nil {
		return nil, err
	}
	c, err := dial(ctx, host, port)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", host, err)
	}
	k.host, k.port, k.deviceName = host, port, name
	return c, nil
}

// Resolve finds the target device: the configured host, or mDNS discovery
// matched by name.
func Resolve(ctx context.Context, cfg config.Cast) (host string, port int, name string, err error) {
	return resolve(ctx, cfg, nil)
}

func resolve(ctx context.Context, cfg config.Cast, discover func(context.Context, time.Duration) ([]cast.Device, error)) (string, int, string, error) {
	if cfg.Host != "" {
		return cfg.Host, cfg.Port, cfg.Host, nil
	}
	if discover == nil {
		discover = cast.Discover
	}
	devs, err := discover(ctx, cfg.Timings().DiscoverTimeout)
	if err != nil {
		return "", 0, "", fmt.Errorf("discover: %w", err)
	}
	d, err := pickDevice(devs, cfg.Device)
	if err != nil {
		return "", 0, "", err
	}
	return d.Host, d.Port, d.Name, nil
}

// pickDevice matches want (case-insensitively) against the devices: an exact
// name first, then the UUID, then a name or model substring. Several
// matches at the same level take the first and log a warning.
func pickDevice(devs []cast.Device, want string) (cast.Device, error) {
	want = strings.ToLower(strings.TrimSpace(want))
	uuid := func(s string) string { return strings.ReplaceAll(strings.ToLower(s), "-", "") }
	tiers := []func(d cast.Device) bool{
		func(d cast.Device) bool { return want == "" },
		func(d cast.Device) bool { return strings.ToLower(d.Name) == want },
		func(d cast.Device) bool { return d.UUID != "" && uuid(d.UUID) == uuid(want) },
		func(d cast.Device) bool {
			return strings.Contains(strings.ToLower(d.Name), want) || strings.Contains(strings.ToLower(d.Model), want)
		},
	}
	if want != "" {
		tiers = tiers[1:]
	}
	for _, match := range tiers {
		var found []cast.Device
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
	return cast.Device{}, fmt.Errorf("no cast device matching %q (found: %s)", want, deviceNames(devs))
}

func deviceNames(devs []cast.Device) string {
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
	defer func() { _ = c.Close() }()

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

// Device is the part of *cast.Client the keeper uses.
type Device interface {
	Status(ctx context.Context) (*cast.ReceiverStatus, error)
	Stop(ctx context.Context, sessionID string) error
	Launch(ctx context.Context, appID string) (*cast.App, error)
	LoadURL(ctx context.Context, url string, force bool, reloadSeconds int) (*cast.App, error)
}

// appLevel reports a device refusing a command (it answered, the
// connection is fine) and swallows it; anything else is returned so the
// connection is rebuilt.
func (k *Keeper) appLevel(what string, err error) error {
	var re *cast.ResponseError
	if errors.As(err, &re) {
		k.report("%s: %v", what, err)
		return nil
	}
	return fmt.Errorf("%s: %w", what, err)
}

// check runs one round: see what the device shows, then close the dashboard
// for the quiet hours, leave it, or (re)cast it.
func (k *Keeper) check(ctx context.Context, c Device) error {
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
	running, ourApp, ours, idle := scr.running, scr.ourApp, scr.ours, scr.idle
	if quiet {
		return k.closeForQuiet(ctx, c, t, scr, why)
	}

	want := k.URL
	if k.Cfg.DashCast() {
		if want, err = k.target(); err != nil {
			k.report("not casting: %v", err)
			return nil
		}
	}
	switch {
	case ours && k.loadedOnce && (!k.Cfg.DashCast() || want == k.shown):
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
		name := running.DisplayName
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
	if k.Cfg.DashCast() && !k.pageAnswers(ctx, want, t.ProbeTimeout) {
		return nil
	}
	lctx, cancel := context.WithTimeout(ctx, t.LaunchTimeout)
	defer cancel()
	sess, err := Show(lctx, c, k.Cfg, want)
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
	running *cast.App // nil when nothing runs
	ourApp  bool      // running is the configured app
	ours    bool      // ... in the session this process launched (or adopted)
	idle    bool      // nothing, the backdrop or an idle screen
}

// observe reads the device status and works out whose app is on screen.
func (k *Keeper) observe(ctx context.Context, c Device, t config.Timings) (screen, error) {
	sctx, cancel := context.WithTimeout(ctx, t.CommandTimeout)
	st, err := c.Status(sctx)
	cancel()
	if err != nil {
		return screen{}, fmt.Errorf("status: %w", err)
	}
	// Normally one app runs. If the device lists several, pick
	// deterministically: ours (the configured app id) if present, else the
	// first one listed.
	var s screen
	if s.running = st.App(k.Cfg.App()); s.running == nil && len(st.Apps) > 0 {
		s.running = &st.Apps[0]
	}
	// Ownership: a session this process launched is ours. Our app with any
	// other session id belongs to another sender, except at the first check
	// after Mantel Cast starts, when it is assumed to be ours from before the
	// restart and adopted (reloaded once, closed during quiet).
	s.ourApp = s.running != nil && s.running.AppID == k.Cfg.App()
	if s.ourApp && !k.checked && k.session == "" {
		k.session = s.running.SessionID
	}
	k.checked = true
	s.ours = s.ourApp && k.session != "" && s.running.SessionID == k.session
	s.idle = s.running == nil || s.running.AppID == cast.AppBackdrop || s.running.IsIdleScreen
	return s, nil
}

// closeForQuiet closes our dashboard for the quiet hours; other apps are left alone.
func (k *Keeper) closeForQuiet(ctx context.Context, c Device, t config.Timings, s screen, why string) error {
	if s.ours {
		// Hand the screen back so the device shows its own dimmed night
		// clock; the dashboard comes back when the quiet ends.
		stctx, cancel := context.WithTimeout(ctx, t.CommandTimeout)
		err := c.Stop(stctx, s.running.SessionID)
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

// pageAnswers probes the page before DashCast casts it and reports why not
// when it does not answer: DashCast shows "Force loading …" forever for a
// page it cannot reach, then the device gives up; don't put that on screen.
func (k *Keeper) pageAnswers(ctx context.Context, want string, timeout time.Duration) bool {
	check := k.Reachable
	if check == nil {
		check = reachable
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	err := check(rctx, stripQuery(want)) // no token: a 401 still proves it is up
	cancel()
	switch {
	case err == nil:
		return true
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

// reachable reports whether anything answers HTTP at u. Any status counts
// (401 without a token, 404 on someone else's site): only "nobody there"
// is a failure. Certificates are verified like the device will: a page
// the Hub refuses is not worth casting.
func reachable(ctx context.Context, u string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// certErr reports whether err is a TLS certificate verification failure.
func certErr(err error) bool {
	var cv *tls.CertificateVerificationError
	return errors.As(err, &cv)
}

// stripQuery drops the query (the access token) from u.
func stripQuery(u string) string {
	if p, err := url.Parse(u); err == nil {
		p.RawQuery = ""
		return p.String()
	}
	return u
}

// Show puts the dashboard on screen: DashCast loads the URL; a custom
// receiver app is simply launched (its own page is the dashboard).
// It returns the session id of the app now showing it.
func Show(ctx context.Context, c Device, cfg config.Cast, url string) (string, error) {
	if cfg.DashCast() {
		// force=true navigates the receiver itself to the page instead of an
		// iframe, which lets plain-http LAN URLs load. It also means DashCast's
		// own script is gone afterwards, so a second load message to a running
		// session is silently dropped: stop it and start a fresh one.
		st, err := c.Status(ctx)
		if err != nil {
			return "", err
		}
		if a := st.App(cast.AppDashCast); a != nil {
			if err := c.Stop(ctx, a.SessionID); err != nil {
				return "", fmt.Errorf("stop old session: %w", err)
			}
			if _, err := c.Launch(ctx, cast.AppDashCast); err != nil {
				return "", err
			}
		}
		a, err := c.LoadURL(ctx, url, true, cfg.Reload)
		if err != nil {
			return "", err
		}
		return a.SessionID, nil
	}
	a, err := c.Launch(ctx, cfg.App())
	if err != nil {
		return "", err
	}
	return a.SessionID, nil
}
