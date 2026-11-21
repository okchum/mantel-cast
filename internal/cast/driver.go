package cast

import (
	"context"
	"fmt"
	"time"

	"github.com/okchum/mantel-cast/internal/display"
)

// Options say what the Cast driver keeps on screen.
type Options struct {
	AppID  string // receiver app; "" = DashCast
	Reload int    // DashCast's reload_seconds (0 = never)
}

func (o Options) app() string {
	if o.AppID == "" {
		return AppDashCast
	}
	return o.AppID
}

// Driver is Google Cast (Nest Hub, Chromecast, Cast-enabled TVs) as a
// display.Driver.
type Driver struct{ Options }

var _ display.Driver = Driver{}

func (Driver) Name() string { return "cast" }

// LoadsPage: DashCast loads the given URL; a Custom Receiver shows its own
// page (its URL is set in the Cast developer console).
func (d Driver) LoadsPage() bool { return d.app() == AppDashCast }

func (Driver) Discover(ctx context.Context, timeout time.Duration) ([]display.Device, error) {
	devs, err := Discover(ctx, timeout)
	out := make([]display.Device, 0, len(devs))
	for _, d := range devs {
		out = append(out, display.Device{Name: d.Name, Model: d.Model, ID: d.UUID, Host: d.Host, Port: d.Port})
	}
	return out, err
}

func (d Driver) Dial(ctx context.Context, host string, port int) (display.Session, error) {
	c, err := Dial(ctx, host, port)
	if err != nil {
		return nil, err
	}
	return NewSession(c, host, d.Options), nil
}

// Conn is the part of *Client a session uses (tests fake it).
type Conn interface {
	Status(ctx context.Context) (*ReceiverStatus, error)
	Stop(ctx context.Context, sessionID string) error
	Launch(ctx context.Context, appID string) (*App, error)
	Send(ctx context.Context, transportID, namespace string, payload any) error
	Done() <-chan struct{}
	Close() error
}

// NewSession wraps a connection to the device at host as a display.Session.
func NewSession(c Conn, host string, o Options) display.Session {
	return &session{c: c, host: host, o: o}
}

type session struct {
	c    Conn
	host string
	o    Options
}

func (s *session) Done() <-chan struct{} { return s.c.Done() }
func (s *session) Close() error          { return s.c.Close() }

// Observe reads the receiver status. Normally one app runs; if the device
// lists several, ours (the configured app id) is picked wherever it is
// listed, else the first one.
func (s *session) Observe(ctx context.Context) (display.Screen, error) {
	st, err := s.c.Status(ctx)
	if err != nil {
		return display.Screen{}, err
	}
	var sc display.Screen
	run := st.App(s.o.app())
	if run == nil && len(st.Apps) > 0 {
		run = &st.Apps[0]
	}
	if run != nil {
		sc.Showing, sc.Handle, sc.Ours = run.DisplayName, run.SessionID, run.AppID == s.o.app()
	}
	sc.Idle = run == nil || run.AppID == AppBackdrop || run.IsIdleScreen
	for _, a := range st.Apps {
		sc.Detail = append(sc.Detail, fmt.Sprintf("%s  %q  idle=%v  %s", a.AppID, a.DisplayName, a.IsIdleScreen, a.StatusText))
	}
	return sc, nil
}

// Show puts the dashboard on screen: DashCast loads url; a Custom Receiver is
// simply launched (its own page is the dashboard). It returns the session
// id of the app now showing it.
func (s *session) Show(ctx context.Context, url string) (string, error) {
	if s.o.app() != AppDashCast {
		a, err := s.c.Launch(ctx, s.o.app())
		if err != nil {
			return "", err
		}
		return a.SessionID, nil
	}
	// force=true navigates the receiver itself to the page instead of an
	// iframe, which lets plain-http LAN URLs load. It also means DashCast's
	// own script is gone afterwards, so a second load message to a running
	// session is silently dropped: stop it and start a fresh one.
	st, err := s.c.Status(ctx)
	if err != nil {
		return "", err
	}
	if a := st.App(AppDashCast); a != nil {
		if err := s.c.Stop(ctx, a.SessionID); err != nil {
			return "", fmt.Errorf("stop old session: %w", err)
		}
	}
	a, err := s.c.Launch(ctx, AppDashCast)
	if err != nil {
		return "", err
	}
	if err := s.c.Send(ctx, a.TransportID, NamespaceDashCast, loadPayload(url, true, s.o.Reload)); err != nil {
		return "", err
	}
	return a.SessionID, nil
}

// Hide stops the app session; the device goes back to its ambient screen
// (or its dimmed night clock).
func (s *session) Hide(ctx context.Context, handle string) error {
	return s.c.Stop(ctx, handle)
}

// NightMode reads the device's night mode from its setup API.
func (s *session) NightMode(ctx context.Context) (*display.NightMode, error) {
	return FetchNightMode(ctx, s.host)
}

// Identify reads the device's name and UUID from its setup API.
func (s *session) Identify(ctx context.Context) (display.Device, error) {
	return Identify(ctx, s.host)
}

// Is makes a ResponseError match display.ErrRefused: the device answered
// and refused this one command.
func (e *ResponseError) Is(target error) bool { return target == display.ErrRefused }
