// Package fully drives Fully Kiosk Browser (an Android kiosk browser, often
// on a wall tablet or a Fire tablet) through its Remote Admin REST API, as a
// display.Driver: the dashboard is a page Fully loads, the quiet hours turn
// the screen off.
//
// Remote Admin must be enabled in Fully (a PLUS licence feature) with a
// password, and reachable from this machine on port 2323. Fully does not
// announce itself on the LAN, so its host must be configured.
package fully

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/okchum/mantel-cast/internal/display"
)

// DefaultPort is Fully's Remote Admin port.
const DefaultPort = 2323

// fullyPackage prefixes Fully Kiosk's Android package names.
const fullyPackage = "de.ozerov.fully"

// Driver is Fully Kiosk Browser as a display.Driver.
type Driver struct {
	Password string // Remote Admin password
	// Client is the HTTP client; nil = one that ignores HTTP(S)_PROXY (the
	// tablet is on the LAN). Requests are bounded by their ctx.
	Client *http.Client
}

var _ display.Driver = Driver{}

func (Driver) Name() string    { return "fully" }
func (Driver) LoadsPage() bool { return true }

func (Driver) Discover(context.Context, time.Duration) ([]display.Device, error) {
	return nil, display.ErrNoDiscovery
}

var defaultClient = func() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	return &http.Client{Transport: t}
}()

// Dial checks that Fully answers at host (with the password) and returns a
// session. The REST API is plain request/response: there is no connection
// to lose, so a failed request ends the session instead.
func (d Driver) Dial(ctx context.Context, host string, port int) (display.Session, error) {
	if port == 0 {
		port = DefaultPort
	}
	c := d.Client
	if c == nil {
		c = defaultClient
	}
	s := &session{
		base:     "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/",
		password: d.Password,
		client:   c,
		done:     make(chan struct{}),
	}
	if _, err := s.info(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Info is the part of Fully's device info the driver reads.
type Info struct {
	DeviceID      string `json:"deviceID"`
	DeviceName    string `json:"deviceName"`
	ScreenOn      bool   `json:"screenOn"`
	CurrentPage   string `json:"currentPage"`
	StartURL      string `json:"startUrl"`
	ForegroundApp string `json:"foregroundApp"`
	AppVersion    string `json:"appVersionName"`
}

type session struct {
	base, password string
	client         *http.Client

	mu    sync.Mutex
	shown string // URL Show last loaded ("" = none yet: adopt what is there)

	once sync.Once
	done chan struct{}
}

// handle names what the session shows: Fully is one browser, so there is
// only ever one.
const handle = "fully"

func (s *session) Done() <-chan struct{} { return s.done }

func (s *session) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

// cmd runs one REST command (legacy "?cmd=" form, understood by every Fully
// version) and decodes the JSON reply into v. Errors never contain the URL:
// it carries the password.
func (s *session) cmd(ctx context.Context, name string, params url.Values, v any) error {
	q := url.Values{}
	for k, vs := range params {
		q[k] = vs
	}
	q.Set("cmd", name)
	q.Set("type", "json")
	q.Set("password", s.password)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"?"+q.Encode(), nil)
	if err != nil {
		return fmt.Errorf("fully %s: %w", name, unwrapURL(err))
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("fully %s: %w", name, unwrapURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("fully %s: %w", name, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fully %s: HTTP %d", name, resp.StatusCode)
	}
	var st struct {
		Status string `json:"status"`
		Text   string `json:"statustext"`
	}
	if json.Unmarshal(body, &st) == nil && strings.EqualFold(st.Status, "error") {
		return &RefusedError{Cmd: name, Text: st.Text}
	}
	if v == nil {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("fully %s: %w", name, err)
	}
	return nil
}

// RefusedError is Fully answering a command with an error status (wrong
// password, a feature that needs a licence or a permission).
type RefusedError struct{ Cmd, Text string }

func (e *RefusedError) Error() string { return fmt.Sprintf("fully %s: %s", e.Cmd, e.Text) }

// Is makes it match display.ErrRefused.
func (e *RefusedError) Is(target error) bool { return target == display.ErrRefused }

func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func (s *session) info(ctx context.Context) (*Info, error) {
	var in Info
	if err := s.cmd(ctx, "deviceInfo", nil, &in); err != nil {
		return nil, err
	}
	return &in, nil
}

// Observe: the screen off, or Fully on its start page or a blank one, is
// idle; another app in front is someone else's; Fully showing the page last
// loaded (any page at first, to adopt it after a restart) is ours.
func (s *session) Observe(ctx context.Context) (display.Screen, error) {
	in, err := s.info(ctx)
	if err != nil {
		return display.Screen{}, err
	}
	s.mu.Lock()
	shown := s.shown
	s.mu.Unlock()
	page := redact(in.CurrentPage)
	sc := display.Screen{Handle: handle, Detail: []string{
		fmt.Sprintf("Fully %s  screen on=%v  foreground=%s  page=%s", in.AppVersion, in.ScreenOn, in.ForegroundApp, page),
	}}
	fullyFront := in.ForegroundApp == "" || strings.HasPrefix(in.ForegroundApp, fullyPackage)
	switch {
	case !in.ScreenOn:
		sc.Showing, sc.Idle = "screen off", true
	case !fullyFront:
		sc.Showing = in.ForegroundApp
	case in.CurrentPage == "" || in.CurrentPage == "about:blank" || (in.StartURL != "" && samePage(in.CurrentPage, in.StartURL) && (shown == "" || !samePage(shown, in.StartURL))):
		sc.Showing, sc.Idle = "Fully start page", true
	case shown == "" || samePage(in.CurrentPage, shown):
		sc.Showing, sc.Ours = "Fully "+page, true
	default:
		sc.Showing = "Fully " + page
	}
	return sc, nil
}

// Show wakes the screen, brings Fully to the front and loads url.
func (s *session) Show(ctx context.Context, u string) (string, error) {
	for _, c := range []string{"screenOn", "toForeground"} {
		if err := s.cmd(ctx, c, nil, nil); err != nil {
			return "", err
		}
	}
	if err := s.cmd(ctx, "loadUrl", url.Values{"url": {u}}, nil); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.shown = u
	s.mu.Unlock()
	return handle, nil
}

// Hide turns the screen off.
func (s *session) Hide(ctx context.Context, _ string) error {
	return s.cmd(ctx, "screenOff", nil, nil)
}

func (s *session) Identify(ctx context.Context) (display.Device, error) {
	in, err := s.info(ctx)
	if err != nil {
		return display.Device{}, err
	}
	return display.Device{Name: in.DeviceName, ID: in.DeviceID, Model: "Fully Kiosk " + in.AppVersion}, nil
}

// samePage compares two page addresses without their query and fragment
// (the token, reload markers) and trailing slash.
func samePage(a, b string) bool { return pageKey(a) == pageKey(b) }

func pageKey(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return u
	}
	return strings.ToLower(p.Scheme+"://"+p.Host) + strings.TrimSuffix(p.EscapedPath(), "/")
}

// redact drops the query (it may carry the access token) for logs.
func redact(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		return u[:i] + "?…"
	}
	return u
}
