// Package ha drives a browser that Home Assistant controls through the
// browser_mod integration (a wall tablet running the HA app, Fully Kiosk or
// any browser with HA open), as a display.Driver.
//
// browser_mod can only move such a browser between Home Assistant's own
// pages, so the dashboard reaches the screen as a Home Assistant "Webpage"
// dashboard that frames the Mantel page: Show navigates the browser to that
// dashboard's path, Hide navigates it home (or turns the screen off). All of
// it goes through Home Assistant's REST API with a long-lived access token.
package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/okchum/mantel-cast/internal/display"
)

// Driver is a browser_mod browser as a display.Driver.
type Driver struct {
	URL   string // Home Assistant, e.g. http://homeassistant.local:8123
	Token string // long-lived access token
	// Path is the Home Assistant path of the dashboard that frames Mantel
	// ("/mantel"); Show navigates there.
	Path string
	// Home is where Hide navigates ("/lovelace/0"); "" turns the screen
	// off instead.
	Home string
	// Client is the HTTP client; nil = http.DefaultClient. Requests are
	// bounded by their ctx.
	Client *http.Client
}

var _ display.Driver = Driver{}

func (Driver) Name() string { return "ha" }

// LoadsPage: the dashboard Show opens frames the Mantel page, so the page is
// still probed before showing it.
func (Driver) LoadsPage() bool { return true }

// entity is one entry of GET /api/states.
type entity struct {
	ID    string `json:"entity_id"`
	State string `json:"state"`
	Attr  struct {
		Type         string   `json:"type"`
		BrowserID    string   `json:"browserID"`
		PathSegments []string `json:"pathSegments"`
		ActivityType *string  `json:"activityType"`
	} `json:"attributes"`
}

func (e entity) browserMod() bool { return e.Attr.Type == "browser_mod" && e.Attr.BrowserID != "" }

// entities are the browser_mod entities of one browser the driver reads.
type entities struct {
	path, activity, screen string // entity ids; activity may be ""
}

func (e entities) complete() bool { return e.path != "" && e.screen != "" }

// classify sorts a browser's entities: the path sensor carries
// pathSegments, the activity sensor activityType, the screen is its light.
func classify(all []entity) map[string]*entities {
	out := map[string]*entities{}
	for _, e := range all {
		if !e.browserMod() {
			continue
		}
		b := out[e.Attr.BrowserID]
		if b == nil {
			b = &entities{}
			out[e.Attr.BrowserID] = b
		}
		switch {
		case strings.HasPrefix(e.ID, "light."):
			b.screen = e.ID
		case strings.HasPrefix(e.ID, "sensor.") && e.Attr.PathSegments != nil:
			b.path = e.ID
		case strings.HasPrefix(e.ID, "binary_sensor.") && e.Attr.ActivityType != nil:
			b.activity = e.ID
		}
	}
	return out
}

type client struct {
	base, token string
	http        *http.Client
}

func (d Driver) client() client {
	c := d.Client
	if c == nil {
		c = http.DefaultClient
	}
	return client{base: strings.TrimSuffix(d.URL, "/"), token: d.Token, http: c}
}

// do sends one REST request and decodes the JSON reply into v (nil = none).
func (c client) do(ctx context.Context, method, path string, body, v any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return fmt.Errorf("home assistant: %w", unwrapURL(err))
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("home assistant: %w", unwrapURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	// /api/states lists every entity: allow a big house.
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("home assistant %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return &StatusError{Path: path, Code: resp.StatusCode, Text: firstLine(b)}
	}
	if v == nil {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("home assistant %s: %w", path, err)
	}
	return nil
}

// StatusError is Home Assistant answering with an error status: 401 a bad
// token, 400 a service browser_mod does not have, 404 a removed entity.
type StatusError struct {
	Path string
	Code int
	Text string
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("home assistant %s: HTTP %d", e.Path, e.Code)
	if e.Code == http.StatusUnauthorized {
		msg += " (check the access token)"
	}
	if e.Text != "" {
		msg += ": " + e.Text
	}
	return msg
}

// Is makes a service call Home Assistant refused match display.ErrRefused:
// the connection is fine, the call is retried on the next check.
func (e *StatusError) Is(target error) bool {
	return target == display.ErrRefused && strings.HasPrefix(e.Path, "/api/services/") && e.Code >= 400 && e.Code < 500
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func (c client) states(ctx context.Context) ([]entity, error) {
	var all []entity
	if err := c.do(ctx, http.MethodGet, "/api/states", nil, &all); err != nil {
		return nil, err
	}
	return all, nil
}

func (c client) state(ctx context.Context, id string) (entity, error) {
	var e entity
	err := c.do(ctx, http.MethodGet, "/api/states/"+url.PathEscape(id), nil, &e)
	return e, err
}

func (c client) call(ctx context.Context, domain, service string, data map[string]any) error {
	return c.do(ctx, http.MethodPost, "/api/services/"+domain+"/"+service, data, nil)
}

// Discover lists the browsers browser_mod has registered. timeout bounds the
// request.
func (d Driver) Discover(ctx context.Context, timeout time.Duration) ([]display.Device, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	all, err := d.client().states(ctx)
	if err != nil {
		return nil, err
	}
	var devs []display.Device
	for id, b := range classify(all) {
		if !b.complete() {
			continue
		}
		devs = append(devs, display.Device{Name: id, ID: id, Host: id, Model: "browser_mod"})
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].Name < devs[j].Name })
	return devs, nil
}

// Dial finds the entities of the browser whose browser_mod ID is host and
// checks that it is connected. The port is unused.
func (d Driver) Dial(ctx context.Context, host string, _ int) (display.Session, error) {
	if host == "" {
		// browser_mod sends a call without a browser to every browser.
		return nil, errors.New("home assistant: no browser ID")
	}
	if d.Path == "" {
		return nil, errors.New("home assistant: no dashboard path to show")
	}
	c := d.client()
	all, err := c.states(ctx)
	if err != nil {
		return nil, err
	}
	b := classify(all)[host]
	if b == nil || !b.complete() {
		return nil, fmt.Errorf("home assistant: browser_mod has no browser %q (see its Browser ID on the tablet's Browser Mod panel)", host)
	}
	s := &session{c: c, id: host, ents: *b, path: d.Path, home: d.Home, done: make(chan struct{})}
	if _, err := s.Observe(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

type session struct {
	c          client
	id         string
	ents       entities
	path, home string

	once sync.Once
	done chan struct{}
}

// handle names what the session shows: one browser shows one page.
const handle = "ha"

func (s *session) Done() <-chan struct{} { return s.done }

func (s *session) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

// errGone is a browser that is not connected to Home Assistant (its
// entities are unavailable): the session ends and the keeper reconnects.
var errGone = errors.New("the browser is not connected to Home Assistant")

// Observe: the screen off is idle; the dashboard's path is ours; another
// page is someone's while the activity sensor says the screen is being
// used, and idle once it no longer does.
func (s *session) Observe(ctx context.Context) (display.Screen, error) {
	screen, err := s.c.state(ctx, s.ents.screen)
	if err != nil {
		return display.Screen{}, err
	}
	path, err := s.c.state(ctx, s.ents.path)
	if err != nil {
		return display.Screen{}, err
	}
	active := false
	if s.ents.activity != "" {
		a, err := s.c.state(ctx, s.ents.activity)
		if err != nil {
			return display.Screen{}, err
		}
		active = a.State == "on"
	}
	if screen.State == "unavailable" || path.State == "unavailable" {
		return display.Screen{}, fmt.Errorf("%s: %w", s.id, errGone)
	}
	sc := display.Screen{Handle: handle, Detail: []string{
		fmt.Sprintf("browser %s  screen=%s  path=%s  active=%v", s.id, screen.State, path.State, active),
	}}
	switch {
	case screen.State == "off":
		sc.Showing, sc.Idle = "screen off", true
	case samePath(path.State, s.path):
		sc.Showing, sc.Ours = "Home Assistant "+path.State, true
	case active:
		sc.Showing = "Home Assistant " + path.State + " (in use)"
	default:
		sc.Showing, sc.Idle = "Home Assistant "+path.State, true
	}
	return sc, nil
}

// samePath reports whether path is want or a page under it ("/mantel/0").
func samePath(path, want string) bool {
	path, want = strings.TrimSuffix(path, "/"), strings.TrimSuffix(want, "/")
	return path == want || strings.HasPrefix(path, want+"/")
}

// Show turns the screen on and navigates the browser to the dashboard; the
// URL is the one the dashboard frames, set in Home Assistant.
func (s *session) Show(ctx context.Context, _ string) (string, error) {
	if err := s.c.call(ctx, "light", "turn_on", map[string]any{"entity_id": s.ents.screen}); err != nil {
		return "", err
	}
	if err := s.navigate(ctx, s.path); err != nil {
		return "", err
	}
	return handle, nil
}

// Hide navigates home, or turns the screen off without a home.
func (s *session) Hide(ctx context.Context, _ string) error {
	if s.home != "" {
		return s.navigate(ctx, s.home)
	}
	return s.c.call(ctx, "light", "turn_off", map[string]any{"entity_id": s.ents.screen})
}

func (s *session) navigate(ctx context.Context, path string) error {
	return s.c.call(ctx, "browser_mod", "navigate", map[string]any{"browser_id": []string{s.id}, "path": path})
}
