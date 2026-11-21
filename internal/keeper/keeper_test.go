package keeper

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/okchum/mantel-cast/internal/cast"
	"github.com/okchum/mantel-cast/internal/config"
	"github.com/okchum/mantel-cast/internal/display"
)

// fakeDevice is a fake cast.Conn: it records what the keeper asks for; apps
// is what Status reports.
type fakeDevice struct {
	apps               []cast.App
	loads              []string
	launch             int
	stopped            int
	sessions           int
	statusErr, loadErr error
	stopErr            error // Stop fails with it and leaves the apps running
}

func (f *fakeDevice) newSession(id string) *cast.App {
	f.sessions++
	s := fmt.Sprintf("s%d", f.sessions)
	f.apps = []cast.App{{AppID: id, SessionID: s, TransportID: s}}
	return &f.apps[0]
}

func (f *fakeDevice) Status(context.Context) (*cast.ReceiverStatus, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return &cast.ReceiverStatus{Apps: append([]cast.App(nil), f.apps...)}, nil
}
func (f *fakeDevice) Stop(context.Context, string) error {
	f.stopped++
	if f.stopErr != nil {
		return f.stopErr
	}
	f.apps = nil
	return nil
}
func (f *fakeDevice) Launch(_ context.Context, id string) (*cast.App, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	f.launch++
	return f.newSession(id), nil
}
func (f *fakeDevice) Send(_ context.Context, transportID, ns string, payload any) error {
	if len(f.apps) != 1 || f.apps[0].TransportID != transportID || ns != cast.NamespaceDashCast {
		return fmt.Errorf("send to %q/%s: no such app", transportID, ns)
	}
	f.loads = append(f.loads, fmt.Sprint(payload.(map[string]any)["url"]))
	return nil
}
func (f *fakeDevice) Done() <-chan struct{} { return nil }
func (f *fakeDevice) Close() error          { return nil }

// plain hides the optional display interfaces (Identifier, NightModer) of
// the real cast session: their setup API reads would go to the network.
type plain struct{ display.Session }

// sess wraps a fake connection the way the cast driver wraps a real one.
func sess(k *Keeper, c cast.Conn) display.Session {
	return plain{cast.NewSession(c, "127.0.0.1", cast.Options{AppID: k.Cfg.AppID, Reload: k.Cfg.Reload})}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) add(d time.Duration)     { c.t = c.t.Add(d) }
func up(context.Context, string) error   { return nil }
func down(context.Context, string) error { return errors.New("connection refused") }

func newKeeper(url string) (*Keeper, *fakeDevice, *clock) {
	clk := &clock{time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}
	k := &Keeper{Cfg: config.Cast{AppID: cast.AppDashCast}, URL: url, Loc: time.UTC, Reachable: up, Now: clk.now, host: "127.0.0.1",
		NightMode: func(context.Context, string) (*display.NightMode, error) { return &display.NightMode{}, nil }}
	return k, &fakeDevice{}, clk
}

func mustCheck(t *testing.T, k *Keeper, d cast.Conn) {
	t.Helper()
	if err := k.check(context.Background(), sess(k, d)); err != nil {
		t.Fatal(err)
	}
}

func TestCastsWhenIdleAndStays(t *testing.T) {
	k, d, _ := newKeeper("http://10.0.0.5:8080/?k=x")
	d.apps = []cast.App{{AppID: cast.AppBackdrop, IsIdleScreen: true}}
	mustCheck(t, k, d)
	mustCheck(t, k, d)
	if len(d.loads) != 1 || d.loads[0] != "http://10.0.0.5:8080/?k=x" || k.lastMsg != "showing" {
		t.Fatalf("loads %v, state %q", d.loads, k.lastMsg)
	}
}

func TestLeavesOtherAppsAlone(t *testing.T) {
	k, d, _ := newKeeper("http://10.0.0.5:8080/")
	d.apps = []cast.App{{AppID: "CC1AD845", DisplayName: "Spotify"}}
	mustCheck(t, k, d)
	if len(d.loads) != 0 || k.lastMsg != "busy: Spotify" {
		t.Fatalf("loads %v, state %q", d.loads, k.lastMsg)
	}
}

func TestDoesNotCastUnreachablePage(t *testing.T) {
	k, d, _ := newKeeper("http://10.0.0.5:8080/?k=secret")
	k.Reachable = down
	mustCheck(t, k, d)
	if len(d.loads) != 0 || !strings.Contains(k.lastMsg, "does not answer") || strings.Contains(k.lastMsg, "secret") {
		t.Fatalf("loads %v, state %q", d.loads, k.lastMsg)
	}
	k.Reachable = up
	mustCheck(t, k, d)
	if len(d.loads) != 1 {
		t.Fatal("should cast once the page answers")
	}
}

func TestAutoHostFollowsAddress(t *testing.T) {
	k, d, _ := newKeeper("http://auto:8080/?k=x")
	mustCheck(t, k, d)
	if len(d.loads) != 1 || d.loads[0] != "http://127.0.0.1:8080/?k=x" {
		t.Fatalf("loads %v", d.loads)
	}
	// Same address: nothing to do.
	mustCheck(t, k, d)
	if len(d.loads) != 1 {
		t.Fatalf("reloaded without a change: %v", d.loads)
	}
	// This machine's address changed while the page was up: reload it.
	k.shown = "http://10.9.9.180:8080/?k=x"
	mustCheck(t, k, d)
	if len(d.loads) != 2 || d.stopped != 1 {
		t.Fatalf("loads %v stopped %d", d.loads, d.stopped)
	}
}

func TestPausesCrashLoop(t *testing.T) {
	k, d, clk := newKeeper("http://10.0.0.5:8080/")
	mustCheck(t, k, d) // first cast
	vanish := func() { d.apps = nil; clk.add(2 * time.Minute); mustCheck(t, k, d) }
	vanish()
	vanish()
	if len(d.loads) != 3 {
		t.Fatalf("want 2 relaunches, loads %d", len(d.loads))
	}
	vanish() // third relaunch inside 15 minutes: pause instead
	if len(d.loads) != 3 || !strings.HasPrefix(k.lastMsg, "paused until 12:16") {
		t.Fatalf("loads %d, state %q", len(d.loads), k.lastMsg)
	}
	clk.add(5 * time.Minute)
	mustCheck(t, k, d)
	if len(d.loads) != 3 {
		t.Fatal("cast during pause")
	}
	clk.add(6 * time.Minute)
	mustCheck(t, k, d)
	if len(d.loads) != 4 || k.lastMsg != "showing" {
		t.Fatalf("after pause: loads %d, state %q", len(d.loads), k.lastMsg)
	}
	// Keeps failing: the next pause is longer.
	vanish()
	vanish()
	vanish()
	if k.pauseStep != 20*time.Minute {
		t.Fatalf("pause step %v", k.pauseStep)
	}
	// A long stable run resets it.
	clk.add(21 * time.Minute)
	mustCheck(t, k, d)
	clk.add(31 * time.Minute)
	mustCheck(t, k, d)
	if k.pauseStep != 0 {
		t.Fatalf("pause step not reset: %v", k.pauseStep)
	}
}

func TestReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("k") {
		case "good":
			// Mantel sets its cookie and serves the page; a redirect (to a
			// path without ?k=) is not followed, it already proves the token.
			if r.URL.Path == "/old" {
				http.Redirect(w, r, "/", http.StatusFound)
				return
			}
			_, _ = w.Write([]byte("ok"))
		case "":
			if r.URL.Path == "/gone" {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "no", http.StatusUnauthorized)
		default:
			http.Error(w, "no", http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	for _, u := range []string{srv.URL + "/?k=good", srv.URL + "/old?k=good"} {
		if err := reachable(ctx, u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	for u, want := range map[string]string{
		srv.URL + "/?k=bad": "rejected the token",
		srv.URL + "/":       "needs a token",
		srv.URL + "/gone":   "answers HTTP 404",
	} {
		var se *statusError
		if err := reachable(ctx, u); !errors.As(err, &se) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", u, err, want)
		}
	}
	// A rejected token is reported and not cast, without the token in the log.
	k, d, _ := newKeeper(srv.URL + "/?k=bad")
	k.Reachable = nil
	mustCheck(t, k, d)
	if len(d.loads) != 0 || !strings.Contains(k.lastMsg, "rejected the token") || strings.Contains(k.lastMsg, "bad") {
		t.Fatalf("loads %v state %q", d.loads, k.lastMsg)
	}
	srv.Close()
	if err := reachable(context.Background(), srv.URL); err == nil {
		t.Fatal("closed server reported reachable")
	}
}

func TestRedact(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"http://h:1/p?k=secret", "http://h:1/p?k=xxx"},
		{"https://h/?k=secret&receiver=1", "https://h/?k=xxx&receiver=1"},
		{"http://h/", "http://h/"},
		{"http://h:bad/?k=secret", "http://h:bad/?…"},
		{"http://[::1%zz/x?k=secret#frag", "http://[::1%zz/x?…"},
	} {
		if got := Redact(c.in); got != c.want {
			t.Errorf("Redact(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// captureLogs collects logf output for the test.
func captureLogs(t *testing.T) *[]string {
	t.Helper()
	var lines []string
	old := logf
	logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	t.Cleanup(func() { logf = old })
	return &lines
}

func TestOtherAppsDoNotCountAsCrashes(t *testing.T) {
	k, d, clk := newKeeper("http://10.0.0.5:8080/")
	mustCheck(t, k, d) // cast
	for i := range 4 {
		// Someone watches YouTube for a minute, then it ends and the device idles.
		d.apps = []cast.App{{AppID: "233637DE", DisplayName: "YouTube", SessionID: fmt.Sprintf("yt%d", i)}}
		clk.add(time.Minute)
		mustCheck(t, k, d)
		if k.lastMsg != "busy: YouTube" {
			t.Fatalf("state %q", k.lastMsg)
		}
		d.apps = nil
		clk.add(time.Minute)
		mustCheck(t, k, d)
	}
	if len(d.loads) != 5 || len(k.relaunches) != 0 || !k.pauseUntil.IsZero() {
		t.Fatalf("loads %d relaunches %d pause %v", len(d.loads), len(k.relaunches), k.pauseUntil)
	}
}

func TestOnlyQuickExitsCountAsCrashes(t *testing.T) {
	k, d, clk := newKeeper("http://10.0.0.5:8080/")
	mustCheck(t, k, d)
	for range 4 {
		// Shown for 5 minutes (seen up by the checks), then the session ends.
		for range 10 {
			clk.add(30 * time.Second)
			mustCheck(t, k, d)
		}
		d.apps = nil
		clk.add(30 * time.Second)
		mustCheck(t, k, d)
	}
	if len(d.loads) != 5 || len(k.relaunches) != 0 {
		t.Fatalf("loads %d relaunches %d", len(d.loads), len(k.relaunches))
	}
	// A longer crash_if_up_under makes the same exits count; relaunch_limit 2
	// pauses after the second.
	k.Cfg.CrashIfUpUnder = config.Duration{Duration: 10 * time.Minute}
	k.Cfg.RelaunchLimit = 2
	k.Cfg.PauseMin = config.Duration{Duration: 3 * time.Minute}
	for range 2 {
		clk.add(5 * time.Minute)
		mustCheck(t, k, d)
		d.apps = nil
		clk.add(30 * time.Second)
		mustCheck(t, k, d)
	}
	if k.pauseStep != 3*time.Minute || !strings.HasPrefix(k.lastMsg, "paused until") {
		t.Fatalf("pause step %v, state %q", k.pauseStep, k.lastMsg)
	}
}

func TestPauseWithoutLocation(t *testing.T) {
	k, d, clk := newKeeper("http://10.0.0.5:8080/")
	k.Loc = nil
	mustCheck(t, k, d)
	for range 3 {
		d.apps = nil
		clk.add(time.Minute)
		mustCheck(t, k, d) // must not panic on k.Loc == nil
	}
	if !strings.HasPrefix(k.lastMsg, "paused until") {
		t.Fatalf("state %q", k.lastMsg)
	}
}

func TestSeveralAppsPicksOursThenFirst(t *testing.T) {
	// Ours is found wherever it is listed: still showing, nothing reloaded.
	k, d, _ := newKeeper("http://10.0.0.5:8080/")
	mustCheck(t, k, d) // casts
	ours := d.apps[0]
	d.apps = []cast.App{{AppID: "CC1AD845", DisplayName: "Spotify"}, ours, {AppID: cast.AppBackdrop, IsIdleScreen: true}}
	mustCheck(t, k, d)
	if len(d.loads) != 1 || k.lastMsg != "showing" {
		t.Fatalf("ours listed second: loads %d state %q", len(d.loads), k.lastMsg)
	}
	// Otherwise the first listed decides, whatever the order of later ones.
	d.apps = []cast.App{{AppID: "CC1AD845", DisplayName: "Spotify"}, {AppID: cast.AppBackdrop, IsIdleScreen: true}}
	mustCheck(t, k, d)
	if len(d.loads) != 1 || k.lastMsg != "busy: Spotify" {
		t.Fatalf("spotify first: loads %d state %q", len(d.loads), k.lastMsg)
	}
}

func TestOwnership(t *testing.T) {
	// Mantel Cast restarted with its page up: adopt the session and reload once.
	k, d, _ := newKeeper("http://10.0.0.5:8080/")
	d.apps = []cast.App{{AppID: cast.AppDashCast, DisplayName: "DashCast", SessionID: "old"}}
	mustCheck(t, k, d)
	if len(d.loads) != 1 || d.stopped != 1 {
		t.Fatalf("restart: loads %d stopped %d", len(d.loads), d.stopped)
	}
	mustCheck(t, k, d)
	if len(d.loads) != 1 || k.lastMsg != "showing" {
		t.Fatalf("loads %d state %q", len(d.loads), k.lastMsg)
	}

	// Someone else casts a page with DashCast later: theirs, left alone…
	d.apps = []cast.App{{AppID: cast.AppDashCast, DisplayName: "DashCast", SessionID: "phone"}}
	mustCheck(t, k, d)
	if len(d.loads) != 1 || d.stopped != 1 || k.lastMsg != "busy: DashCast (cast by someone else)" {
		t.Fatalf("foreign: loads %d stopped %d state %q", len(d.loads), d.stopped, k.lastMsg)
	}
	// …also when the quiet starts.
	k.Cfg.QuietHours = "00:00-24:00"
	mustCheck(t, k, d)
	if d.stopped != 1 || len(d.apps) != 1 {
		t.Fatal("closed someone else's DashCast for the quiet")
	}
}

func TestQuietAtStartClosesLeftoverPage(t *testing.T) {
	k, d, _ := newKeeper("http://10.0.0.5:8080/")
	k.Cfg.QuietHours = "00:00-24:00"
	d.apps = []cast.App{{AppID: cast.AppDashCast, DisplayName: "DashCast", SessionID: "old"}}
	mustCheck(t, k, d)
	if d.stopped != 1 || len(d.apps) != 0 || k.session != "" {
		t.Fatalf("stopped %d apps %v session %q", d.stopped, d.apps, k.session)
	}
}

func TestQuietStopFailure(t *testing.T) {
	k, d, _ := newKeeper("http://10.0.0.5:8080/")
	k.Cfg.QuietHours = "00:00-24:00"
	d.apps = []cast.App{{AppID: cast.AppDashCast, DisplayName: "DashCast", SessionID: "old"}}
	// The device refuses the stop: reported, the connection kept, and the
	// page still counts as ours so the next check tries again.
	d.stopErr = &cast.ResponseError{Op: "STOP", Type: "INVALID_REQUEST"}
	if err := k.check(context.Background(), sess(k, d)); err != nil {
		t.Fatalf("app-level stop error tore down the session: %v", err)
	}
	if d.stopped != 1 || k.session != "old" || !strings.Contains(k.lastMsg, "stop for quiet") || !strings.Contains(k.lastMsg, "INVALID_REQUEST") {
		t.Fatalf("refused: stopped %d session %q state %q", d.stopped, k.session, k.lastMsg)
	}
	// A connection error ends the session so it is rebuilt.
	d.stopErr = errors.New("cast: write: broken pipe")
	if err := k.check(context.Background(), sess(k, d)); err == nil || !strings.Contains(err.Error(), "stop for quiet") {
		t.Fatalf("connection error during the quiet stop: %v", err)
	}
	if k.session != "old" {
		t.Fatalf("session %q dropped after a failed stop", k.session)
	}
	d.stopErr = nil
	mustCheck(t, k, d)
	if d.stopped != 3 || len(d.apps) != 0 || k.session != "" || !strings.HasPrefix(k.lastMsg, "quiet") {
		t.Fatalf("retry: stopped %d apps %v session %q state %q", d.stopped, d.apps, k.session, k.lastMsg)
	}
}

func TestEmptyAppIDMeansDashCast(t *testing.T) {
	// A cast section that skipped Normalize still keeps DashCast.
	k, d, _ := newKeeper("http://10.0.0.5:8080/")
	k.Cfg.AppID = ""
	mustCheck(t, k, d)
	mustCheck(t, k, d)
	if len(d.loads) != 1 || d.launch != 1 || k.lastMsg != "showing" {
		t.Fatalf("loads %v launches %d state %q", d.loads, d.launch, k.lastMsg)
	}
}

func TestCustomReceiver(t *testing.T) {
	k, d, _ := newKeeper("")
	k.Cfg.AppID = "ABCD1234"
	k.Reachable = down // not consulted: the receiver's page is its own
	mustCheck(t, k, d)
	mustCheck(t, k, d)
	if len(d.loads) != 0 || d.launch != 1 || len(d.apps) != 1 || d.apps[0].AppID != "ABCD1234" || k.lastMsg != "showing" {
		t.Fatalf("loads %v launches %d apps %v state %q", d.loads, d.launch, d.apps, k.lastMsg)
	}
}

func TestDeviceRefusalKeepsConnection(t *testing.T) {
	k, d, _ := newKeeper("http://10.0.0.5:8080/")
	d.loadErr = &cast.ResponseError{Op: "LAUNCH", Type: "LAUNCH_ERROR"}
	if err := k.check(context.Background(), sess(k, d)); err != nil {
		t.Fatalf("app-level error tore down the session: %v", err)
	}
	if !strings.Contains(k.lastMsg, "LAUNCH_ERROR") {
		t.Fatalf("state %q", k.lastMsg)
	}
	d.loadErr = errors.New("cast: write: broken pipe")
	if err := k.check(context.Background(), sess(k, d)); err == nil {
		t.Fatal("connection error should end the session")
	}
	d.statusErr = errors.New("cast: GET_STATUS: context deadline exceeded")
	if err := k.check(context.Background(), sess(k, d)); err == nil {
		t.Fatal("status error should end the session")
	}
}

func TestStatusGetsItsOwnDeadline(t *testing.T) {
	k, d, _ := newKeeper("http://10.0.0.5:8080/")
	k.Cfg.CommandTimeout = config.Duration{Duration: time.Second}
	var phoneDone time.Time
	k.Phone = func(context.Context) (*Asleep, error) {
		time.Sleep(300 * time.Millisecond)
		phoneDone = time.Now()
		return nil, nil
	}
	sd := &deadlineDevice{fakeDevice: d}
	mustCheck(t, k, sd)
	if left := sd.deadline.Sub(phoneDone); left < 900*time.Millisecond {
		t.Fatalf("status had only %v after the quiet reads", left)
	}
}

type deadlineDevice struct {
	*fakeDevice
	deadline time.Time
}

func (d *deadlineDevice) Status(ctx context.Context) (*cast.ReceiverStatus, error) {
	if d.deadline.IsZero() {
		d.deadline, _ = ctx.Deadline()
	}
	return d.fakeDevice.Status(ctx)
}

func TestBadCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	err := reachable(context.Background(), srv.URL)
	if err == nil || !certErr(err) {
		t.Fatalf("self-signed page: %v", err)
	}
	k, d, _ := newKeeper(srv.URL + "/?k=secret")
	k.Reachable = nil
	mustCheck(t, k, d)
	if len(d.loads) != 0 || !strings.Contains(k.lastMsg, "bad certificate") || strings.Contains(k.lastMsg, "secret") {
		t.Fatalf("loads %v state %q", d.loads, k.lastMsg)
	}
}

func TestResolveAuto(t *testing.T) {
	for _, u := range []string{"https://automation.example.com/", "http://auto.local:8080/", "http://10.0.0.5/?k=x"} {
		if got, err := ResolveAuto(u, "127.0.0.1"); err != nil || got != u {
			t.Errorf("ResolveAuto(%q) = %q, %v; want unchanged", u, got, err)
		}
	}
	if got, err := ResolveAuto("http://auto:8080/?k=x", "127.0.0.1"); err != nil || got != "http://127.0.0.1:8080/?k=x" {
		t.Errorf("auto with port: %q %v", got, err)
	}
	if got, err := ResolveAuto("https://AUTO/", "127.0.0.1"); err != nil || got != "https://AUTO/" {
		// url.Hostname keeps the case; only lower-case "auto" is special.
		t.Errorf("upper case: %q %v", got, err)
	}
	if got, err := ResolveAuto("http://auto/", "127.0.0.1"); err != nil || got != "http://127.0.0.1/" {
		t.Errorf("auto without port: %q %v", got, err)
	}
	_, err := ResolveAuto("http://auto:bad/?k=secret", "127.0.0.1")
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("bad URL error leaks the token: %v", err)
	}
}

func TestPickDevice(t *testing.T) {
	logs := captureLogs(t)
	devs := []display.Device{
		{Name: "Kitchen Display Max", Model: "Google Nest Hub Max", ID: "aaaa-1"},
		{Name: "Kitchen Display", Model: "Google Nest Hub", ID: "bbbb-2"},
		{Name: "Living Room TV", Model: "Chromecast", ID: "cccc-3"},
	}
	for want, name := range map[string]string{
		"kitchen display": "Kitchen Display", // exact beats substring (and sort order)
		"CCCC3":           "Living Room TV",  // UUID, dashes ignored
		"chromecast":      "Living Room TV",  // model substring
		"":                "Kitchen Display Max",
	} {
		d, err := pickDevice(devs, want)
		if err != nil || d.Name != name {
			t.Errorf("pickDevice(%q) = %q, %v; want %q", want, d.Name, err, name)
		}
	}
	*logs = nil
	if d, _ := pickDevice(devs, "nest hub"); d.Name != "Kitchen Display Max" || len(*logs) != 1 || !strings.Contains((*logs)[0], "2 devices match") {
		t.Errorf("ambiguous: %q, logs %v", d.Name, *logs)
	}
	if _, err := pickDevice(devs, "bedroom"); err == nil || !strings.Contains(err.Error(), `"Living Room TV"`) {
		t.Errorf("no match: %v", err)
	}
}

func TestBackoff(t *testing.T) {
	var b backoff
	lasted := []time.Duration{0, time.Second, time.Second, time.Second, time.Second, time.Second, time.Second, 10 * time.Minute, time.Second}
	got := make([]time.Duration, 0, len(lasted))
	for _, l := range lasted {
		got = append(got, b.next(l))
	}
	want := fmt.Sprint([]time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second, 5 * time.Second, 10 * time.Second})
	if fmt.Sprint(got) != want {
		t.Fatalf("backoff %v, want %s", got, want)
	}
}

type fakeConn struct {
	*fakeDevice
	done chan struct{}
}

func (c *fakeConn) Done() <-chan struct{} { return c.done }
func (c *fakeConn) Close() error          { return nil }

func TestConnectTriesLastHostFirst(t *testing.T) {
	captureLogs(t)
	k, _, _ := newKeeper("http://10.0.0.5:8080/")
	k.host = ""
	var dialed []string
	discovered := 0
	up := map[string]bool{"10.0.0.7": true}
	k.dial = func(_ context.Context, host string, _ int) (display.Session, error) {
		dialed = append(dialed, host)
		if !up[host] {
			return nil, errors.New("refused")
		}
		return sess(k, &fakeConn{&fakeDevice{}, make(chan struct{})}), nil
	}
	k.discover = func(context.Context, time.Duration) ([]display.Device, error) {
		discovered++
		return []display.Device{{Name: "Hub", Host: "10.0.0.7", Port: 8009}}, nil
	}
	ctx := context.Background()
	if _, err := k.connect(ctx); err != nil || discovered != 1 || k.host != "10.0.0.7" {
		t.Fatalf("first connect: %v, discovered %d host %q", err, discovered, k.host)
	}
	if _, err := k.connect(ctx); err != nil || discovered != 1 {
		t.Fatalf("reconnect should dial the known host without discovery: %v, discovered %d", err, discovered)
	}
	// The device moved: the known address fails, discovery finds it again.
	up = map[string]bool{"10.0.0.9": true}
	k.discover = func(context.Context, time.Duration) ([]display.Device, error) {
		discovered++
		return []display.Device{{Name: "Hub", Host: "10.0.0.9", Port: 8009}}, nil
	}
	if _, err := k.connect(ctx); err != nil || discovered != 2 || k.host != "10.0.0.9" {
		t.Fatalf("moved: %v, discovered %d host %q", err, discovered, k.host)
	}
	if fmt.Sprint(dialed) != "[10.0.0.7 10.0.0.7 10.0.0.7 10.0.0.9]" {
		t.Fatalf("dialed %v", dialed)
	}
}

func TestRunResetsBackoffAfterHealthySession(t *testing.T) {
	captureLogs(t)
	k, _, clk := newKeeper("http://10.0.0.5:8080/")
	k.Cfg.Host = "10.0.0.7"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sessions := 0
	k.dial = func(context.Context, string, int) (display.Session, error) {
		sessions++
		c := &fakeConn{&fakeDevice{}, make(chan struct{})}
		if sessions == 3 {
			clk.add(time.Hour) // this one stays up for an hour
		}
		close(c.done) // then the connection drops
		return sess(k, c), nil
	}
	var waits []time.Duration
	k.sleep = func(_ context.Context, d time.Duration) bool {
		waits = append(waits, d)
		if len(waits) == 4 {
			cancel()
			return false
		}
		return true
	}
	k.Run(ctx)
	if fmt.Sprint(waits) != fmt.Sprint([]time.Duration{5 * time.Second, 10 * time.Second, 5 * time.Second, 10 * time.Second}) {
		t.Fatalf("waits %v", waits)
	}
}

// idSession is a session whose device tells who it is.
type idSession struct {
	display.Session
	dev display.Device
	err error
}

func (s idSession) Identify(context.Context) (display.Device, error) { return s.dev, s.err }

func TestReconnectChecksIdentity(t *testing.T) {
	logs := captureLogs(t)
	k, _, _ := newKeeper("http://10.0.0.5:8080/")
	k.host = ""
	at := map[string]display.Device{"10.0.0.7": {Name: "Hub", ID: "AAAA-1"}}
	var idErr error
	var dialed []string
	k.dial = func(_ context.Context, host string, _ int) (display.Session, error) {
		dialed = append(dialed, host)
		return idSession{sess(k, &fakeConn{&fakeDevice{}, make(chan struct{})}), at[host], idErr}, nil
	}
	discovered := 0
	k.discover = func(context.Context, time.Duration) ([]display.Device, error) {
		discovered++
		var out []display.Device
		for h, d := range at {
			d.Host = h
			out = append(out, d)
		}
		return out, nil
	}
	ctx := context.Background()
	if _, err := k.connect(ctx); err != nil || k.deviceID != "AAAA-1" {
		t.Fatalf("first: %v id %q", err, k.deviceID)
	}
	// Same device (the id in another spelling): reused without discovery.
	at["10.0.0.7"] = display.Device{Name: "Hub", ID: "aaaa1"}
	if _, err := k.connect(ctx); err != nil || discovered != 1 {
		t.Fatalf("same device: %v discovered %d", err, discovered)
	}
	// Cannot tell (the setup API did not answer): keep it.
	idErr = errors.New("timeout")
	if _, err := k.connect(ctx); err != nil || discovered != 1 {
		t.Fatalf("unknown identity: %v discovered %d", err, discovered)
	}
	idErr = nil
	// DHCP gave the address to another Hub and ours moved: discover again.
	at = map[string]display.Device{"10.0.0.7": {Name: "Kitchen", ID: "BBBB-2"}, "10.0.0.9": {Name: "Hub", ID: "AAAA-1"}}
	k.Cfg.Device = "Hub"
	if _, err := k.connect(ctx); err != nil || discovered != 2 || k.host != "10.0.0.9" {
		t.Fatalf("moved: %v discovered %d host %q", err, discovered, k.host)
	}
	if !strings.Contains(strings.Join(*logs, "\n"), `now answers as "Kitchen"`) {
		t.Fatalf("logs %v", *logs)
	}
	if fmt.Sprint(dialed) != "[10.0.0.7 10.0.0.7 10.0.0.7 10.0.0.7 10.0.0.9]" {
		t.Fatalf("dialed %v", dialed)
	}
}

// fakeScreen is a display.Session of a generic page-loading device.
type fakeScreen struct {
	sc     display.Screen
	shows  []string
	hidden int
}

func (f *fakeScreen) Observe(context.Context) (display.Screen, error) { return f.sc, nil }
func (f *fakeScreen) Show(_ context.Context, u string) (string, error) {
	f.shows = append(f.shows, u)
	f.sc = display.Screen{Showing: "page", Handle: "h", Ours: true}
	return "h", nil
}
func (f *fakeScreen) Hide(context.Context, string) error {
	f.hidden++
	f.sc = display.Screen{Showing: "screen off", Handle: "h", Idle: true}
	return nil
}
func (f *fakeScreen) Done() <-chan struct{} { return nil }
func (f *fakeScreen) Close() error          { return nil }

type pageDriver struct{ display.Driver }

func (pageDriver) LoadsPage() bool { return true }

func TestOtherDriver(t *testing.T) {
	// The keeper logic is the same for any driver: show when idle, leave a
	// foreign app alone, hide for the quiet. No night mode of its own: the
	// quiet comes from the hours (and phone) only.
	k, _, _ := newKeeper("http://10.0.0.5:8080/?k=x")
	k.Driver, k.NightMode = pageDriver{}, nil
	f := &fakeScreen{sc: display.Screen{Showing: "screen off", Handle: "h", Idle: true}}
	k.sess = f
	ctx := context.Background()
	if err := k.check(ctx, f); err != nil || fmt.Sprint(f.shows) != "[http://10.0.0.5:8080/?k=x]" || k.lastMsg != "showing" {
		t.Fatalf("show: %v %v %q", err, f.shows, k.lastMsg)
	}
	if err := k.check(ctx, f); err != nil || len(f.shows) != 1 {
		t.Fatalf("stays: %v %v", err, f.shows)
	}
	f.sc = display.Screen{Showing: "com.netflix.ninja"}
	if err := k.check(ctx, f); err != nil || len(f.shows) != 1 || k.lastMsg != "busy: com.netflix.ninja" {
		t.Fatalf("busy: %v %v %q", err, f.shows, k.lastMsg)
	}
	f.sc = display.Screen{Showing: "page", Handle: "h", Ours: true}
	k.Cfg.QuietHours = "00:00-24:00"
	if err := k.check(ctx, f); err != nil || f.hidden != 1 || !strings.HasPrefix(k.lastMsg, "quiet") {
		t.Fatalf("quiet: %v hidden %d %q", err, f.hidden, k.lastMsg)
	}
	if k.hub != nil || k.hubErr != "" {
		t.Fatalf("night mode read from a driver without one: %+v %q", k.hub, k.hubErr)
	}
}
