package keeper

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/okchum/mantel-cast/internal/cast"
	"github.com/okchum/mantel-cast/internal/config"
)

func TestDecide(t *testing.T) {
	now := time.Date(2026, 5, 4, 23, 0, 0, 0, time.UTC) // Monday 23:00
	night := &cast.NightMode{Enabled: true, Windows: []cast.NightWindow{{StartHour: 22, LengthHours: 8}}}
	off := &cast.NightMode{Enabled: false, Windows: night.Windows}
	dnd := &cast.NightMode{DoNotDisturb: true}
	asleep := &Asleep{On: true, At: now.Add(-time.Hour)}
	awake := &Asleep{On: false, At: now.Add(-3 * time.Hour)}
	cases := []struct {
		name string
		mode string
		in   QuietInputs
		want bool
		why  string
	}{
		{"nothing", "auto", QuietInputs{}, false, ""},
		{"hub night mode", "auto", QuietInputs{Hub: night}, true, "night mode on the device 22:00+8h"},
		{"hub window but disabled", "auto", QuietInputs{Hub: off}, false, ""},
		{"dnd", "auto", QuietInputs{Hub: dnd}, true, "do not disturb"},
		{"phone asleep", "auto", QuietInputs{Phone: asleep}, true, "phone asleep since 22:00"},
		{"phone awake beats schedule", "auto", QuietInputs{Phone: awake, Hub: night, Hours: "22:00-07:00"}, false, ""},
		{"dnd beats phone awake", "auto", QuietInputs{Phone: awake, Hub: dnd}, true, "do not disturb"},
		{"stale sleep report ignored", "auto", QuietInputs{Phone: &Asleep{On: true, At: now.Add(-20 * time.Hour)}}, false, ""},
		{"old phone falls back to hours", "auto", QuietInputs{Phone: &Asleep{At: now.Add(-48 * time.Hour)}, Hours: "22:30-07:00"}, true, "quiet hours"},
		{"hours fallback", "auto", QuietInputs{Hours: "22:30-07:00"}, true, "quiet hours 22:30-07:00"},
		{"hours mode ignores hub", "hours", QuietInputs{Hub: dnd}, false, ""},
		{"off", "off", QuietInputs{Hub: dnd, Hours: "00:00-23:59"}, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.in.Loc = time.UTC
			got, why := Decide(c.mode, c.in, now)
			if got != c.want || !strings.HasPrefix(why, c.why) {
				t.Fatalf("got %v %q", got, why)
			}
		})
	}
	// The Hub's own zone is used for its windows.
	nz := *night
	nz.Timezone = "Pacific/Auckland" // 23:00 UTC = 11:00 NZST: daytime there
	if q, _ := Decide("auto", QuietInputs{Hub: &nz, Loc: time.UTC}, now); q {
		t.Fatal("window should be read in the device's zone")
	}
}

func TestQuietClosesAndReturns(t *testing.T) {
	k, d, clk := newKeeper("http://10.0.0.5:8080/")
	var phone *Asleep
	k.Phone = func(context.Context) (*Asleep, error) { return phone, nil }
	mustCheck(t, k, d)
	if len(d.loads) != 1 {
		t.Fatal("not cast")
	}
	phone = &Asleep{On: true, At: clk.t}
	mustCheck(t, k, d)
	if len(d.apps) != 0 || d.stopped != 1 || !strings.HasPrefix(k.lastMsg, "quiet (phone asleep") {
		t.Fatalf("apps %v stopped %d state %q", d.apps, d.stopped, k.lastMsg)
	}
	clk.add(time.Minute)
	mustCheck(t, k, d) // stays quiet, nothing to stop
	if len(d.loads) != 1 || d.stopped != 1 {
		t.Fatal("touched the device while quiet")
	}
	clk.add(7 * time.Hour)
	phone = &Asleep{On: false, At: clk.t}
	mustCheck(t, k, d)
	if len(d.loads) != 2 || k.lastMsg != "showing" {
		t.Fatalf("not back in the morning: loads %d state %q", len(d.loads), k.lastMsg)
	}
	if len(k.relaunches) != 0 {
		t.Fatal("coming back after quiet must not count as a relaunch")
	}
}

func TestQuietLeavesOtherApps(t *testing.T) {
	k, d, _ := newKeeper("http://10.0.0.5:8080/")
	k.Cfg.QuietHours = "00:00-23:59"
	d.apps = []cast.App{{AppID: "CC1AD845", DisplayName: "Spotify", SessionID: "x"}}
	mustCheck(t, k, d)
	if d.stopped != 0 || len(d.apps) != 1 {
		t.Fatal("stopped someone else's app")
	}
}

func TestHubReadingCached(t *testing.T) {
	k, _, clk := newKeeper("http://x/")
	fail := false
	k.NightMode = func(context.Context, string) (*cast.NightMode, error) {
		if fail {
			return nil, errors.New("timeout")
		}
		return &cast.NightMode{DoNotDisturb: true}, nil
	}
	if k.hubNight(context.Background(), clk.t) == nil {
		t.Fatal("no reading")
	}
	fail = true
	clk.add(30 * time.Minute)
	if n := k.hubNight(context.Background(), clk.t); n == nil || !n.DoNotDisturb {
		t.Fatal("blip should use the cached reading")
	}
	clk.add(time.Hour)
	if k.hubNight(context.Background(), clk.t) != nil {
		t.Fatal("cache should expire")
	}
}

func TestNightModeRefreshAndRecovery(t *testing.T) {
	logs := captureLogs(t)
	k, _, clk := newKeeper("http://x/")
	fetches := 0
	var fail error
	k.NightMode = func(context.Context, string) (*cast.NightMode, error) {
		fetches++
		if fail != nil {
			return nil, fail
		}
		return &cast.NightMode{}, nil
	}
	ctx := context.Background()
	k.hubNight(ctx, clk.t)
	clk.add(4 * time.Minute)
	k.hubNight(ctx, clk.t)
	if fetches != 1 {
		t.Fatalf("re-read within night_mode_refresh: %d fetches", fetches)
	}
	clk.add(2 * time.Minute)
	fail = errors.New("timeout")
	k.hubNight(ctx, clk.t)
	clk.add(6 * time.Minute)
	k.hubNight(ctx, clk.t)
	fail = nil
	clk.add(6 * time.Minute)
	k.hubNight(ctx, clk.t)
	clk.add(6 * time.Minute)
	k.hubNight(ctx, clk.t)
	if fetches != 5 || k.hubErr != "" {
		t.Fatalf("fetches %d, hubErr %q", fetches, k.hubErr)
	}
	want := "[quiet: cannot read the device's night mode: timeout quiet: the device's night mode is readable again]"
	if fmt.Sprint(*logs) != want {
		t.Fatalf("logs %v", *logs)
	}
	// A custom refresh is honoured.
	k.Cfg.NightModeRefresh = config.Duration{Duration: time.Minute}
	clk.add(90 * time.Second)
	k.hubNight(ctx, clk.t)
	if fetches != 6 {
		t.Fatalf("night_mode_refresh 1m: %d fetches", fetches)
	}
}

func TestPhoneErrorRecovery(t *testing.T) {
	logs := captureLogs(t)
	k, _, clk := newKeeper("http://x/")
	var err error
	k.Phone = func(context.Context) (*Asleep, error) { return nil, err }
	err = errors.New("HTTP 502")
	k.quietNow(context.Background(), clk.t)
	k.quietNow(context.Background(), clk.t)
	err = nil
	k.quietNow(context.Background(), clk.t)
	k.quietNow(context.Background(), clk.t)
	if len(*logs) != 2 || !strings.Contains((*logs)[1], "readable again") || k.phoneErr != "" {
		t.Fatalf("logs %v", *logs)
	}
}

func TestQuietReadsRunTogether(t *testing.T) {
	k, _, clk := newKeeper("http://x/")
	phoneIn, hubIn := make(chan struct{}), make(chan struct{})
	wait := func(ch chan struct{}) error {
		select {
		case <-ch:
			return nil
		case <-time.After(2 * time.Second):
			return errors.New("the other read never started")
		}
	}
	k.Phone = func(context.Context) (*Asleep, error) { close(phoneIn); return nil, wait(hubIn) }
	k.NightMode = func(context.Context, string) (*cast.NightMode, error) {
		close(hubIn)
		return &cast.NightMode{}, wait(phoneIn)
	}
	k.quietNow(context.Background(), clk.t)
	if k.phoneErr != "" || k.hubErr != "" {
		t.Fatalf("reads ran one after the other: %q %q", k.phoneErr, k.hubErr)
	}
}

func TestDecideCustomTTLs(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	asleep := &Asleep{On: true, At: now.Add(-5 * time.Hour)}
	if q, _ := Decide("auto", QuietInputs{Phone: asleep, Loc: time.UTC}, now); !q {
		t.Fatal("default 16h TTL")
	}
	if q, _ := Decide("auto", QuietInputs{Phone: asleep, Loc: time.UTC, SleepTTL: 4 * time.Hour}, now); q {
		t.Fatal("sleep_report_ttl 4h should expire a 5h-old report")
	}
	awake := &Asleep{At: now.Add(-3 * time.Hour)}
	if q, _ := Decide("auto", QuietInputs{Phone: awake, Hours: "00:00-23:59", Loc: time.UTC, PhoneKnownFor: 2 * time.Hour}, now); !q {
		t.Fatal("phone_known_for 2h: a 3h-old awake report no longer beats quiet_hours")
	}
}

func TestZoneCached(t *testing.T) {
	a := zone("Pacific/Auckland")
	if a == nil || a.String() != "Pacific/Auckland" {
		t.Fatalf("zone = %v", a)
	}
	if b := zone("Pacific/Auckland"); b != a {
		t.Error("second lookup should reuse the cached location")
	}
	if zone("") != nil || zone("Nowhere/Bogus") != nil || zone("Nowhere/Bogus") != nil {
		t.Error("empty or unknown zones should be nil")
	}
}
