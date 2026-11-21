package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/okchum/mantel-cast/internal/cast"
)

func TestAppDashCastMatchesCast(t *testing.T) {
	if AppDashCast != cast.AppDashCast {
		t.Fatalf("config.AppDashCast %q, cast.AppDashCast %q", AppDashCast, cast.AppDashCast)
	}
}

func TestCastApp(t *testing.T) {
	for _, c := range []struct {
		id, app  string
		dashCast bool
	}{
		{"", AppDashCast, true},
		{AppDashCast, AppDashCast, true},
		{"ABCD1234", "ABCD1234", false},
	} {
		cfg := Cast{AppID: c.id}
		if got := cfg.App(); got != c.app || cfg.DashCast() != c.dashCast {
			t.Errorf("AppID %q: App() %q DashCast() %v, want %q %v", c.id, got, cfg.DashCast(), c.app, c.dashCast)
		}
	}
}

func TestCastNormalize(t *testing.T) {
	c := Cast{}
	if err := c.Normalize(); err != nil || c.AppID != "84912283" || c.Quiet != "auto" {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []Cast{{Quiet: "sometimes"}, {QuietHours: "late"}} {
		if err := bad.Normalize(); err == nil {
			t.Errorf("%+v should fail", bad)
		}
	}
}

func TestCastNormalizeDrivers(t *testing.T) {
	c := Cast{}
	if err := c.Normalize(); err != nil || c.Driver != DriverCast {
		t.Fatalf("default driver: %+v %v", c, err)
	}
	c = Cast{Driver: DriverFully, Host: "10.0.0.20", Password: "pw"}
	if err := c.Normalize(); err != nil || c.Driver != DriverFully {
		t.Fatalf("fully: %+v %v", c, err)
	}
	// All day is 00:00-24:00, and the error for an empty window says so.
	c = Cast{QuietHours: "00:00-24:00"}
	if err := c.Normalize(); err != nil {
		t.Fatalf("all day: %v", err)
	}
	c = Cast{QuietHours: "00:00-00:00"}
	if err := c.Normalize(); err == nil || !strings.Contains(err.Error(), "00:00-24:00") {
		t.Fatalf("empty window: %v", err)
	}
}

func TestCastTimings(t *testing.T) {
	c := Cast{}
	if c.Timings() != DefaultTimings {
		t.Fatalf("defaults %+v", c.Timings())
	}
	if DefaultTimings.RelaunchWindow != 15*time.Minute || DefaultTimings.RelaunchLimit != 3 || DefaultTimings.NightModeRefresh != 5*time.Minute {
		t.Fatal("defaults changed")
	}
	var v Cast
	if err := json.Unmarshal([]byte(`{"pause_min":"2m","pause_max":"5m","relaunch_limit":5,"night_mode_refresh":"1m"}`), &v); err != nil {
		t.Fatal(err)
	}
	if err := v.Normalize(); err != nil {
		t.Fatal(err)
	}
	if tm := v.Timings(); tm.PauseMin != 2*time.Minute || tm.PauseMax != 5*time.Minute || tm.RelaunchLimit != 5 ||
		tm.NightModeRefresh != time.Minute || tm.CommandTimeout != 15*time.Second {
		t.Fatalf("%+v", tm)
	}
}

func TestCastNormalizeRejects(t *testing.T) {
	neg := Duration{Duration: -time.Second}
	for name, bad := range map[string]Cast{
		"negative timing":     {ProbeTimeout: neg},
		"negative interval":   {CheckInterval: neg},
		"negative limit":      {RelaunchLimit: -1},
		"pause_min > max":     {PauseMin: Duration{Duration: 2 * time.Hour}},
		"pause_min > set max": {PauseMin: Duration{Duration: 20 * time.Minute}, PauseMax: Duration{Duration: 15 * time.Minute}},
		"empty window":        {QuietHours: "00:00-00:00"},
		"same start and end":  {QuietHours: "07:30-07:30"},
		"negative reload":     {Reload: -1},
		"port out of range":   {Port: 70000},
		"unknown driver":      {Driver: "airplay"},
		"fully with app_id":   {Driver: DriverFully, Host: "h", AppID: "ABCD1234"},
		"fully with reload":   {Driver: DriverFully, Host: "h", Reload: 60},
		"password for cast":   {Password: "pw"},
	} {
		if err := bad.Normalize(); err == nil {
			t.Errorf("%s: %+v should fail", name, bad)
		}
	}
}

func TestCastEnabled(t *testing.T) {
	for body, want := range map[string]bool{`{}`: true, `{"enabled": true}`: true, `{"enabled": false}`: false} {
		var c Cast
		if err := json.Unmarshal([]byte(body), &c); err != nil {
			t.Fatal(err)
		}
		if c.IsEnabled() != want {
			t.Errorf("%s: enabled %v", body, c.IsEnabled())
		}
	}
}

func TestCastMantelTimeout(t *testing.T) {
	for body, want := range map[string]time.Duration{
		`{}`:                       DefaultTimings.MantelTimeout,
		`{"mantel_timeout": "3s"}`: 3 * time.Second,
	} {
		var c Cast
		if err := json.Unmarshal([]byte(body), &c); err != nil {
			t.Fatal(err)
		}
		if err := c.Normalize(); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if got := c.Timings().MantelTimeout; got != want {
			t.Errorf("%s: mantel timeout %s, want %s", body, got, want)
		}
	}
	c := Cast{MantelTimeout: Duration{Duration: -time.Second}}
	if err := c.Normalize(); err == nil || !strings.Contains(err.Error(), "mantel_timeout") {
		t.Fatalf("a negative mantel_timeout must fail: %v", err)
	}
}
