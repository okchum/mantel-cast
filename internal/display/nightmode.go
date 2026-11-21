package display

import (
	"fmt"
	"math"
	"time"
)

// NightMode is a device's own night mode / do-not-disturb setting (a Nest
// Hub's, as set in the Google Home app).
type NightMode struct {
	Enabled      bool          `json:"enabled"`
	DoNotDisturb bool          `json:"do_not_disturb"`
	Windows      []NightWindow `json:"windows"`
	Timezone     string        `json:"-"` // the device's zone, "" = unknown
}

// NightWindow starts at StartHour (local, fractional) on each listed day and
// lasts LengthHours, possibly past midnight.
type NightWindow struct {
	// Days uses 0 = Sunday … 6 = Saturday (like time.Weekday). The default
	// schedule lists every day, so this only matters for custom ones.
	Days        []int   `json:"days"`
	StartHour   float64 `json:"start_hour"`
	LengthHours float64 `json:"length_hours"`
}

// maxNightHours caps a window's length: the values come from the device, and
// a nonsense length must not turn Active into a near-endless loop.
const maxNightHours = 7 * 24

// Active reports whether t falls inside one of the windows (whether or not
// night mode is enabled), and which one. Windows starting outside [0, 24)
// or with no length are ignored; lengths are capped at a week.
func (n *NightMode) Active(t time.Time) (bool, NightWindow) {
	for _, w := range n.Windows {
		if !(w.StartHour >= 0 && w.StartHour < 24 && w.LengthHours > 0) { // also rejects NaN
			continue
		}
		length := min(w.LengthHours, maxNightHours)
		// A window that started on an earlier day may still be running.
		back := int(w.StartHour+length) / 24
		for off := 0; off <= back; off++ {
			d := time.Date(t.Year(), t.Month(), t.Day()-off, 0, 0, 0, 0, t.Location())
			if !hasDay(w.Days, int(d.Weekday())) {
				continue
			}
			// Wall-clock times in t's zone, so a DST change that night does
			// not shift the window by an hour.
			from := int(math.Round(w.StartHour * 60))
			to := from + int(math.Round(length*60))
			start := time.Date(d.Year(), d.Month(), d.Day(), 0, from, 0, 0, d.Location())
			end := time.Date(d.Year(), d.Month(), d.Day(), 0, to, 0, 0, d.Location())
			if !t.Before(start) && t.Before(end) {
				return true, w
			}
		}
	}
	return false, NightWindow{}
}

func hasDay(days []int, d int) bool {
	if len(days) == 0 {
		return true
	}
	for _, x := range days {
		if x == d {
			return true
		}
	}
	return false
}

// String is "22:00+8h" style, for logs.
func (w NightWindow) String() string {
	// Rounded to the minute like Active, so the log shows the real start.
	mins := int(math.Round(w.StartHour * 60))
	return fmt.Sprintf("%02d:%02d+%gh", mins/60, mins%60, w.LengthHours)
}
