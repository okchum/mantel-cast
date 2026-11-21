package cast

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/okchum/mantel-cast/internal/display"
)

// fakeConn is a Conn that runs one app at a time and records the calls.
type fakeConn struct {
	apps  []App
	calls []string
	n     int
}

func (f *fakeConn) Status(context.Context) (*ReceiverStatus, error) {
	f.calls = append(f.calls, "status")
	return &ReceiverStatus{Apps: append([]App(nil), f.apps...)}, nil
}
func (f *fakeConn) Stop(_ context.Context, id string) error {
	f.calls = append(f.calls, "stop "+id)
	f.apps = nil
	return nil
}
func (f *fakeConn) Launch(_ context.Context, id string) (*App, error) {
	f.n++
	f.calls = append(f.calls, "launch "+id)
	s := fmt.Sprintf("s%d", f.n)
	f.apps = []App{{AppID: id, DisplayName: id, SessionID: s, TransportID: "t" + s}}
	return &f.apps[0], nil
}
func (f *fakeConn) Send(_ context.Context, tr, ns string, p any) error {
	m := p.(map[string]any)
	f.calls = append(f.calls, fmt.Sprintf("send %s %s %v %v", tr, m["url"], m["force"], m["reload_time"]))
	return nil
}
func (f *fakeConn) Done() <-chan struct{} { return nil }
func (f *fakeConn) Close() error          { return nil }

func TestSessionShowDashCast(t *testing.T) {
	f := &fakeConn{apps: []App{{AppID: AppDashCast, SessionID: "old"}}}
	s := NewSession(f, "h", Options{Reload: 60})
	h, err := s.Show(context.Background(), "http://m/?k=x")
	if err != nil || h != "s1" {
		t.Fatalf("show: %q %v", h, err)
	}
	// One status read, then stop the old page, a fresh DashCast, the load:
	// no second GET_STATUS between launch and load.
	want := "[status stop old launch " + AppDashCast + " send ts1 http://m/?k=x true 60]"
	if fmt.Sprint(f.calls) != want {
		t.Fatalf("calls %v\nwant  %s", f.calls, want)
	}
}

func TestSessionShowCustomReceiver(t *testing.T) {
	f := &fakeConn{}
	s := NewSession(f, "h", Options{AppID: "ABCD1234"})
	if h, err := s.Show(context.Background(), "ignored"); err != nil || h != "s1" || fmt.Sprint(f.calls) != "[launch ABCD1234]" {
		t.Fatalf("%q %v %v", h, err, f.calls)
	}
	if (Driver{Options{AppID: "ABCD1234"}}).LoadsPage() || !(Driver{}).LoadsPage() {
		t.Fatal("LoadsPage: only DashCast loads the URL")
	}
}

func TestSessionObserve(t *testing.T) {
	f := &fakeConn{}
	s := NewSession(f, "h", Options{})
	ctx := context.Background()
	for _, c := range []struct {
		apps                []App
		showing, handle     string
		ours, idle, details bool
	}{
		{nil, "", "", false, true, false},
		{[]App{{AppID: AppBackdrop, DisplayName: "Backdrop", SessionID: "b"}}, "Backdrop", "b", false, true, true},
		{[]App{{AppID: "X", DisplayName: "Photos", IsIdleScreen: true}}, "Photos", "", false, true, true},
		{[]App{{AppID: "CC1AD845", DisplayName: "Spotify", SessionID: "sp"}}, "Spotify", "sp", false, false, true},
		// Ours wins wherever it is listed.
		{[]App{{AppID: "CC1AD845", DisplayName: "Spotify"}, {AppID: AppDashCast, DisplayName: "DashCast", SessionID: "d"}}, "DashCast", "d", true, false, true},
	} {
		f.apps = c.apps
		sc, err := s.Observe(ctx)
		if err != nil || sc.Showing != c.showing || sc.Handle != c.handle || sc.Ours != c.ours || sc.Idle != c.idle || (len(sc.Detail) > 0) != c.details {
			t.Errorf("%v: %+v %v", c.apps, sc, err)
		}
	}
}

func TestResponseErrorIsRefused(t *testing.T) {
	err := fmt.Errorf("cast: %w", &ResponseError{Op: "LAUNCH", Type: "LAUNCH_ERROR"})
	if !errors.Is(err, display.ErrRefused) {
		t.Fatal("a refused command must match display.ErrRefused")
	}
	if errors.Is(errors.New("broken pipe"), display.ErrRefused) {
		t.Fatal("other errors must not")
	}
}

func TestSetupAPI(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/setup/eureka_info" {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Query().Get("params") {
		case "name,ssdp_udn":
			_, _ = w.Write([]byte(`{"name":"Bedroom Display","ssdp_udn":"0B1C2D3E-AAAA-BBBB-CCCC-112233445566"}`))
		default:
			_, _ = w.Write([]byte(eureka))
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	old := setupPort
	setupPort = port
	t.Cleanup(func() { setupPort = old })

	s := NewSession(&fakeConn{}, host, Options{})
	d, err := s.(display.Identifier).Identify(context.Background())
	if err != nil || d.Name != "Bedroom Display" || d.ID != "0b1c2d3eaaaabbbbcccc112233445566" || d.Host != host {
		t.Fatalf("identify: %+v %v", d, err)
	}
	n, err := s.(display.NightModer).NightMode(context.Background())
	if err != nil || !n.Enabled || n.Timezone != "Pacific/Auckland" {
		t.Fatalf("night mode: %+v %v", n, err)
	}
	srv.Close()
	if _, err := Identify(context.Background(), host); err == nil || !strings.Contains(err.Error(), "connect") {
		t.Fatalf("closed device: %v", err)
	}
}
