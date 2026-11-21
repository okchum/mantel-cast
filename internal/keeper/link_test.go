package keeper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/okchum/mantel-cast/internal/config"
)

func TestWithToken(t *testing.T) {
	tok := PickToken([]config.Token{{Name: "iphone", Token: "aaa"}, {Name: "nesthub", Token: "b+b"}})
	if tok != "b+b" {
		t.Fatalf("PickToken = %q", tok)
	}
	if got := PickToken([]config.Token{{Name: "nesthub", Token: "n"}, {Name: "mantel-cast", Token: "m"}}); got != "m" {
		t.Errorf("mantel-cast should win: %q", got)
	}
	if got := PickToken([]config.Token{{Name: "tv", Token: "t"}}); got != "t" {
		t.Errorf("first token fallback: %q", got)
	}
	cases := map[string]string{
		"http://10.0.0.2:8080/": "http://10.0.0.2:8080/?k=b%2Bb",
		"https://h/?receiver=1": "https://h/?k=b%2Bb&receiver=1",
		"https://h/?k=mine":     "https://h/?k=mine",
		"https://h/?bookmark=1": "https://h/?bookmark=1&k=b%2Bb",
	}
	for in, want := range cases {
		if got := WithToken(in, tok); got != want {
			t.Errorf("WithToken(%q) = %q, want %q", in, got, want)
		}
	}
	if got := WithToken("http://x/", ""); got != "http://x/" {
		t.Errorf("no token: %q", got)
	}
	if got := Redact("https://h/?k=secret&receiver=1"); strings.Contains(got, "secret") {
		t.Errorf("mask leaked: %q", got)
	}
}

// TestMantelQuietContract pins the seam with Mantel: GET /api/v1/quiet
// answers {"asleep": state.Asleep} (on, at, by omitempty) or {"asleep": null}.
func TestMantelQuietContract(t *testing.T) {
	body := ""
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	ctx := context.Background()

	body = `{"asleep":{"on":true,"at":"2026-11-19T23:00:00+13:00","by":"iphone"}}`
	a, err := FetchAsleep(ctx, srv.URL+"/?k=tok")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	if a == nil || !a.On || !a.At.Equal(at) || a.By != "iphone" {
		t.Fatalf("asleep %+v", a)
	}
	if auth != "Bearer tok" || path != "/api/v1/quiet" {
		t.Fatalf("request %q %q", auth, path)
	}

	body = `{"asleep":null}`
	if a, err := FetchAsleep(ctx, srv.URL+"/"); err != nil || a != nil {
		t.Fatalf("no report: %+v %v", a, err)
	}
	// The API resolves against the page like the page's own fetches.
	for page, want := range map[string]string{
		"/index.html?k=tok#top":  "/api/v1/quiet",
		"/dash/?k=tok":           "/dash/api/v1/quiet",
		"/dash/index.html?k=tok": "/dash/api/v1/quiet",
	} {
		if _, err := FetchAsleep(ctx, srv.URL+page); err != nil || path != want || auth != "Bearer tok" {
			t.Fatalf("%s: %q %q %v", page, path, auth, err)
		}
	}
	if _, err := FetchAsleep(ctx, srv.URL+"?k=tok"); err != nil || path != "/api/v1/quiet" {
		t.Fatalf("no path: %q %v", path, err)
	}
	body = `{"asleep":{"on":false,"at":"2026-11-20T07:00:00+13:00"}}`
	if a, err := FetchAsleep(ctx, srv.URL+"/"); err != nil || a == nil || a.On || a.By != "" {
		t.Fatalf("awake without by: %+v %v", a, err)
	}
}

func TestFetchAsleepBadURLHidesToken(t *testing.T) {
	_, err := FetchAsleep(context.Background(), "http://h:bad/?k=secret")
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaks the token: %v", err)
	}
}
