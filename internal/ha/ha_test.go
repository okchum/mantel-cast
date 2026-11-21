package ha

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/okchum/mantel-cast/internal/display"
)

const token = "secret-token"

// fakeHA is Home Assistant with browser_mod: two browsers, entities named
// as browser_mod names them, the service calls recorded.
type fakeHA struct {
	mu     sync.Mutex
	states map[string]map[string]any // entity id → state object
	calls  []string                  // "domain.service {json}"
	reject bool                      // answer service calls 400
}

func newFakeHA() *fakeHA {
	f := &fakeHA{states: map[string]map[string]any{}}
	for _, b := range []struct{ id, slug string }{{"kitchen-tab", "kitchen_tab"}, {"hall-1234", "hall_1234"}} {
		attrs := func(extra map[string]any) map[string]any {
			a := map[string]any{"type": "browser_mod", "browserID": b.id}
			for k, v := range extra {
				a[k] = v
			}
			return a
		}
		f.set("light."+b.slug+"_screen", "on", attrs(map[string]any{"brightness": 255}))
		f.set("sensor."+b.slug+"_browser_path", "/lovelace/0", attrs(map[string]any{"pathSegments": []string{"", "lovelace", "0"}}))
		f.set("binary_sensor."+b.slug, "off", attrs(map[string]any{"activityType": "mouse"}))
		f.set("sensor."+b.slug+"_browser_id", b.id, attrs(nil))
	}
	f.set("light.kitchen_ceiling", "on", map[string]any{"friendly_name": "Ceiling"})
	return f
}

func (f *fakeHA) set(id, state string, attrs map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if attrs == nil {
		attrs = f.states[id]["attributes"].(map[string]any)
	}
	f.states[id] = map[string]any{"entity_id": id, "state": state, "attributes": attrs}
}

func (f *fakeHA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+token {
		http.Error(w, "401: Unauthorized", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/states":
		var all []any
		for _, s := range f.states {
			all = append(all, s)
		}
		_ = json.NewEncoder(w).Encode(all)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/states/"):
		s, ok := f.states[strings.TrimPrefix(r.URL.Path, "/api/states/")]
		if !ok {
			http.Error(w, `{"message": "Entity not found."}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(s)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/services/"):
		if f.reject {
			http.Error(w, "400: Bad Request", http.StatusBadRequest)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		b, _ := json.Marshal(body)
		svc := strings.ReplaceAll(strings.TrimPrefix(r.URL.Path, "/api/services/"), "/", ".")
		f.calls = append(f.calls, svc+" "+string(b))
		_, _ = w.Write([]byte("[]"))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeHA) takeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

func start(t *testing.T) (*fakeHA, Driver) {
	t.Helper()
	f := newFakeHA()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, Driver{URL: srv.URL + "/", Token: token, Path: "/mantel"}
}

func TestDiscoverListsBrowsers(t *testing.T) {
	_, d := start(t)
	devs, err := d.Discover(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 2 || devs[0].ID != "hall-1234" || devs[1].Host != "kitchen-tab" {
		t.Fatalf("devices = %+v", devs)
	}
}

func TestDialUnknownBrowserOrToken(t *testing.T) {
	_, d := start(t)
	if _, err := d.Dial(context.Background(), "nope", 0); err == nil || !strings.Contains(err.Error(), `no browser "nope"`) {
		t.Fatalf("unknown browser: %v", err)
	}
	if _, err := d.Dial(context.Background(), "", 0); err == nil {
		t.Fatal("an empty browser id would address every browser")
	}
	d.Token = "wrong"
	_, err := d.Dial(context.Background(), "kitchen-tab", 0)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusUnauthorized || !strings.Contains(err.Error(), "access token") {
		t.Fatalf("bad token: %v", err)
	}
	if strings.Contains(err.Error(), token) || errors.Is(err, display.ErrRefused) {
		t.Fatalf("bad token error: %v", err)
	}
}

func TestObserveShowHide(t *testing.T) {
	f, d := start(t)
	ctx := context.Background()
	s, err := d.Dial(ctx, "kitchen-tab", 0)
	if err != nil {
		t.Fatal(err)
	}
	observe := func() display.Screen {
		t.Helper()
		sc, err := s.Observe(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return sc
	}
	if sc := observe(); !sc.Idle || sc.Ours || sc.Handle != "ha" {
		t.Fatalf("another dashboard nobody uses = idle: %+v", sc)
	}
	f.set("binary_sensor.kitchen_tab", "on", nil)
	if sc := observe(); sc.Idle || sc.Ours || !strings.Contains(sc.Showing, "in use") {
		t.Fatalf("a dashboard in use = someone else's: %+v", sc)
	}
	f.set("light.kitchen_tab_screen", "off", nil)
	if sc := observe(); !sc.Idle {
		t.Fatalf("screen off = idle: %+v", sc)
	}

	h, err := s.Show(ctx, "http://mantel.lan:8080/?k=t")
	if err != nil || h != "ha" {
		t.Fatal(h, err)
	}
	want := []string{
		`light.turn_on {"entity_id":"light.kitchen_tab_screen"}`,
		`browser_mod.navigate {"browser_id":["kitchen-tab"],"path":"/mantel"}`,
	}
	if got := f.takeCalls(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("show calls:\n%s", strings.Join(got, "\n"))
	}
	f.set("light.kitchen_tab_screen", "on", nil)
	f.set("sensor.kitchen_tab_browser_path", "/mantel/0", nil)
	if sc := observe(); !sc.Ours || sc.Idle || len(sc.Detail) != 1 {
		t.Fatalf("on the dashboard = ours, even while touched: %+v", sc)
	}
	f.set("sensor.kitchen_tab_browser_path", "/mantel-other", nil)
	if sc := observe(); sc.Ours {
		t.Fatalf("a path that only starts the same is not ours: %+v", sc)
	}

	if err := s.Hide(ctx, h); err != nil {
		t.Fatal(err)
	}
	if got := f.takeCalls(); len(got) != 1 || got[0] != `light.turn_off {"entity_id":"light.kitchen_tab_screen"}` {
		t.Fatalf("hide without home: %v", got)
	}
	d.Home = "/lovelace/home"
	s2, err := d.Dial(ctx, "kitchen-tab", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.Hide(ctx, h); err != nil {
		t.Fatal(err)
	}
	if got := f.takeCalls(); len(got) != 1 || got[0] != `browser_mod.navigate {"browser_id":["kitchen-tab"],"path":"/lovelace/home"}` {
		t.Fatalf("hide with home: %v", got)
	}
}

func TestDisconnectedBrowserEndsSession(t *testing.T) {
	f, d := start(t)
	s, err := d.Dial(context.Background(), "hall-1234", 0)
	if err != nil {
		t.Fatal(err)
	}
	f.set("sensor.hall_1234_browser_path", "unavailable", nil)
	if _, err := s.Observe(context.Background()); !errors.Is(err, errGone) {
		t.Fatalf("unavailable entities: %v", err)
	}
	if _, err := d.Dial(context.Background(), "hall-1234", 0); !errors.Is(err, errGone) {
		t.Fatalf("dial a disconnected browser: %v", err)
	}
}

func TestRefusedServiceIsErrRefused(t *testing.T) {
	f, d := start(t)
	s, err := d.Dial(context.Background(), "kitchen-tab", 0)
	if err != nil {
		t.Fatal(err)
	}
	f.reject = true
	if _, err := s.Show(context.Background(), ""); !errors.Is(err, display.ErrRefused) {
		t.Fatalf("a refused service call: %v", err)
	}
}

func TestSamePath(t *testing.T) {
	for _, c := range []struct {
		path, want string
		same       bool
	}{
		{"/mantel", "/mantel", true},
		{"/mantel/0", "/mantel", true},
		{"/mantel/", "/mantel/", true},
		{"/mantel2", "/mantel", false},
		{"/lovelace/0", "/mantel", false},
	} {
		if got := samePath(c.path, c.want); got != c.same {
			t.Errorf("samePath(%q, %q) = %v", c.path, c.want, got)
		}
	}
	if (Driver{}).Name() != "ha" || !(Driver{}).LoadsPage() {
		t.Fatal("name / LoadsPage")
	}
}
