package fully

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/okchum/mantel-cast/internal/display"
)

// fakeFully answers the Remote Admin commands the driver sends.
type fakeFully struct {
	mu   sync.Mutex
	info Info
	cmds []string
}

func (f *fakeFully) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	if q.Get("password") != "pw" {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "Error", "statustext": "Please login"})
		return
	}
	cmd := q.Get("cmd")
	f.cmds = append(f.cmds, cmd)
	switch cmd {
	case "deviceInfo":
		_ = json.NewEncoder(w).Encode(f.info)
		return
	case "screenOn":
		f.info.ScreenOn = true
	case "screenOff":
		f.info.ScreenOn = false
	case "toForeground":
		f.info.ForegroundApp = fullyPackage + ".kiosk"
	case "loadUrl":
		f.info.CurrentPage = q.Get("url")
	default:
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "Error", "statustext": "unknown command"})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "OK", "statustext": "done"})
}

func start(t *testing.T, f *fakeFully, password string) (display.Session, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	host, p, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(p)
	s, err := Driver{Password: password}.Dial(context.Background(), host, port)
	if err != nil {
		return nil, srv
	}
	return s, srv
}

func TestDialChecksPassword(t *testing.T) {
	f := &fakeFully{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	host, p, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(p)
	_, err := Driver{Password: "wrong"}.Dial(context.Background(), host, port)
	if !errors.Is(err, display.ErrRefused) || !strings.Contains(err.Error(), "Please login") || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("wrong password: %v", err)
	}
	srv.Close()
	_, err = Driver{Password: "secret"}.Dial(context.Background(), host, port)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("closed: error must not carry the password: %v", err)
	}
}

func TestShowObserveHide(t *testing.T) {
	f := &fakeFully{info: Info{DeviceID: "abc", DeviceName: "Wall", StartURL: "http://start/", CurrentPage: "http://start/", ForegroundApp: fullyPackage + ".kiosk", ScreenOn: true}}
	s, _ := start(t, f, "pw")
	if s == nil {
		t.Fatal("dial failed")
	}
	ctx := context.Background()
	obs := func() display.Screen {
		t.Helper()
		sc, err := s.Observe(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return sc
	}
	if sc := obs(); !sc.Idle || sc.Ours {
		t.Fatalf("start page: %+v", sc)
	}
	f.info.ScreenOn = false
	if sc := obs(); !sc.Idle || sc.Showing != "screen off" {
		t.Fatalf("screen off: %+v", sc)
	}
	h, err := s.Show(ctx, "http://m:8080/?k=tok")
	if err != nil || h == "" {
		t.Fatalf("show: %q %v", h, err)
	}
	if got := strings.Join(f.cmds[len(f.cmds)-3:], " "); got != "screenOn toForeground loadUrl" {
		t.Fatalf("show sent %s", got)
	}
	sc := obs()
	if !sc.Ours || sc.Idle || sc.Handle != h || strings.Contains(strings.Join(sc.Detail, " ")+sc.Showing, "tok") {
		t.Fatalf("after show: %+v", sc)
	}
	// The page reloaded itself without the token: still ours.
	f.info.CurrentPage = "http://m:8080"
	if sc := obs(); !sc.Ours {
		t.Fatalf("same page: %+v", sc)
	}
	// Someone opened another page, or another app came to the front.
	f.info.CurrentPage = "http://other/"
	if sc := obs(); sc.Ours || sc.Idle {
		t.Fatalf("other page: %+v", sc)
	}
	f.info.ForegroundApp = "com.netflix.ninja"
	if sc := obs(); sc.Ours || sc.Idle || sc.Showing != "com.netflix.ninja" {
		t.Fatalf("other app: %+v", sc)
	}
	if err := s.Hide(ctx, h); err != nil || f.info.ScreenOn {
		t.Fatalf("hide: %v, screen on %v", err, f.info.ScreenOn)
	}
	d, err := s.(display.Identifier).Identify(ctx)
	if err != nil || d.ID != "abc" || d.Name != "Wall" {
		t.Fatalf("identify: %+v %v", d, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("Done not closed by Close")
	}
}

func TestAdoptsPageBeforeFirstShow(t *testing.T) {
	// Mantel Cast restarted: whatever Fully shows (not its start page) is
	// taken as the dashboard from before, like an adopted Cast session.
	f := &fakeFully{info: Info{StartURL: "http://start/", CurrentPage: "http://m:8080/", ForegroundApp: fullyPackage, ScreenOn: true}}
	s, _ := start(t, f, "pw")
	sc, err := s.Observe(context.Background())
	if err != nil || !sc.Ours || sc.Idle {
		t.Fatalf("%+v %v", sc, err)
	}
	// Showing the start page as the dashboard itself counts as ours once shown.
	if _, err := s.Show(context.Background(), "http://start/"); err != nil {
		t.Fatal(err)
	}
	if sc, _ := s.Observe(context.Background()); !sc.Ours || sc.Idle {
		t.Fatalf("dashboard is the start page: %+v", sc)
	}
}

func TestNoDiscovery(t *testing.T) {
	if _, err := (Driver{}).Discover(context.Background(), time.Second); !errors.Is(err, display.ErrNoDiscovery) {
		t.Fatal(err)
	}
	if (Driver{}).Name() != "fully" || !(Driver{}).LoadsPage() {
		t.Fatal("driver identity")
	}
}
