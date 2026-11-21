package cast

import (
	"strings"
	"testing"
	"time"
)

const eureka = `{"name":"Bedroom Display","timezone":"Pacific/Auckland","night_mode_params":{"device_override_do_not_disturb":0,"do_not_disturb":false,"enabled":true,"led_brightness":1.0,"volume":1.0,"windows":[{"days":[0,1,2,3,4,5,6],"length_hours":8.0,"start_hour":22.0}]}}`

func TestParseNightMode(t *testing.T) {
	n, err := parseNightMode(strings.NewReader(eureka))
	if err != nil {
		t.Fatal(err)
	}
	if !n.Enabled || n.DoNotDisturb || n.Timezone != "Pacific/Auckland" || len(n.Windows) != 1 || n.Windows[0].String() != "22:00+8h" {
		t.Fatalf("%+v", n)
	}
	if _, err := parseNightMode(strings.NewReader(`{"name":"x"}`)); err == nil {
		t.Fatal("missing params should fail")
	}
}

func TestNightModeActive(t *testing.T) {
	at := func(day, h, m int) time.Time { return time.Date(2026, 5, day, h, m, 0, 0, time.UTC) } // 2026-05-03 is a Sunday
	n := &NightMode{Windows: []NightWindow{{Days: []int{0, 1, 2, 3, 4, 5, 6}, StartHour: 22, LengthHours: 8}}}
	for _, c := range []struct {
		t    time.Time
		want bool
	}{
		{at(4, 21, 59), false}, {at(4, 22, 0), true}, {at(5, 3, 0), true}, {at(5, 5, 59), true}, {at(5, 6, 0), false}, {at(5, 12, 0), false},
	} {
		if got, _ := n.Active(c.t); got != c.want {
			t.Errorf("%v: got %v", c.t, got)
		}
	}
	// Friday and Saturday nights only (5, 6): Sunday 01:00 belongs to Saturday's window,
	// Monday 01:00 to Sunday's, which is not listed.
	n = &NightMode{Windows: []NightWindow{{Days: []int{5, 6}, StartHour: 23.5, LengthHours: 8}}}
	if ok, _ := n.Active(at(3, 1, 0)); !ok {
		t.Error("Sunday 01:00 should be in Saturday's window")
	}
	if ok, _ := n.Active(at(4, 1, 0)); ok {
		t.Error("Monday 01:00 should not be quiet")
	}
	if ok, _ := n.Active(at(2, 23, 0)); ok {
		t.Error("Saturday 23:00 is before 23:30")
	}
}

func TestNightModeActiveAcrossDST(t *testing.T) {
	akl, err := time.LoadLocation("Pacific/Auckland")
	if err != nil {
		t.Skip(err)
	}
	// 2026-09-27: clocks go from 02:00 to 03:00 in Auckland.
	n := &NightMode{Windows: []NightWindow{{StartHour: 4, LengthHours: 1}}}
	at := func(h, m int) time.Time { return time.Date(2026, 9, 27, h, m, 0, 0, akl) }
	for _, c := range []struct {
		t    time.Time
		want bool
	}{{at(3, 59), false}, {at(4, 0), true}, {at(4, 59), true}, {at(5, 0), false}} {
		if got, _ := n.Active(c.t); got != c.want {
			t.Errorf("%v: got %v", c.t, got)
		}
	}
	// 22:00 for 8 hours on the night the clocks change still ends at 06:00 on the clock.
	n = &NightMode{Windows: []NightWindow{{StartHour: 22, LengthHours: 8}}}
	if ok, _ := n.Active(time.Date(2026, 9, 27, 5, 30, 0, 0, akl)); !ok {
		t.Error("05:30 after the change should still be night")
	}
	if ok, _ := n.Active(time.Date(2026, 9, 27, 6, 0, 0, 0, akl)); ok {
		t.Error("06:00 should be the end")
	}
}

func TestNightWindowStringRounds(t *testing.T) {
	// 22.9833… h is 22:59 when rounded (as Active does), not 22:58.
	w := NightWindow{StartHour: 22 + 59.0/60, LengthHours: 8}
	if got := w.String(); got != "22:59+8h" {
		t.Fatalf("String() = %q", got)
	}
}
