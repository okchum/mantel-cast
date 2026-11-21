// Package config holds Mantel Cast's settings: where the Mantel page is, the token
// to open it with, and which device (Cast, or a kiosk browser) to keep on it.
package config

import (
	"fmt"
	"time"

	"github.com/okchum/mantel-kit/conf"
)

// AppDashCast is DashCast's receiver app id. It equals cast.AppDashCast (a
// test checks); copied so config does not depend on the protocol package.
const AppDashCast = "84912283"

// Drivers: the screen technologies cast.driver can name.
const (
	DriverCast  = "cast"  // Google Cast: Nest Hub, Chromecast, Cast TVs (default)
	DriverFully = "fully" // Fully Kiosk Browser's Remote Admin (Android tablets)
)

// Duration is a time.Duration that unmarshals from strings like "30s" or "5m".
type Duration = conf.Duration

type Cast struct {
	// Enabled = false keeps "mantel-cast run" idle (Mantel's config.json may carry
	// this section for a box that does not run Mantel Cast). Absent = enabled.
	Enabled *bool `json:"enabled"`
	// Driver is the screen technology: "cast" (default) or "fully".
	Driver string `json:"driver"`
	Device string `json:"device"` // name, UUID or name/model substring, matched case-insensitively
	Host   string `json:"host"`   // skip discovery when set (fully: required, it cannot be discovered)
	Port   int    `json:"port"`
	AppID  string `json:"app_id"` // cast: default DashCast; set to your Custom Receiver id to use one
	// Password is Fully's Remote Admin password ("$ENV_NAME" reads the
	// environment, expanded by whoever loads the config).
	Password      string   `json:"password"`
	CheckInterval Duration `json:"check_interval"`
	// TakeOver re-casts even when another app (YouTube, Spotify, ...) is playing.
	// Default false: only re-cast when the device sits on its idle/ambient screen.
	TakeOver   bool   `json:"take_over"`
	QuietHours string `json:"quiet_hours"` // "23:00-07:00": no re-cast inside this local window
	// Quiet picks what makes the keeper go quiet (close the dashboard, don't
	// re-cast): "auto" (default) = Hub do-not-disturb, phone sleep reports,
	// Hub night mode, then quiet_hours; "hours" = quiet_hours only; "off".
	Quiet  string `json:"quiet"`
	Reload int    `json:"reload_seconds"`

	// Optional timings; zero or absent = the default in Timings.
	CrashIfUpUnder    Duration `json:"crash_if_up_under"`    // gone sooner than this after a cast counts as a crash
	RelaunchWindow    Duration `json:"relaunch_window"`      // relaunch_limit crashes inside this window…
	RelaunchLimit     int      `json:"relaunch_limit"`       // …pause re-casting
	PauseMin          Duration `json:"pause_min"`            // first pause; doubles each time
	PauseMax          Duration `json:"pause_max"`            // longest pause
	StableAfter       Duration `json:"stable_after"`         // showing this long resets the pause step
	SleepReportTTL    Duration `json:"sleep_report_ttl"`     // a phone "asleep" report counts this long
	PhoneKnownFor     Duration `json:"phone_known_for"`      // any report this recent means "a phone is reporting"
	NightModeRefresh  Duration `json:"night_mode_refresh"`   // re-read the device's night mode this often
	NightModeStaleFor Duration `json:"night_mode_stale_for"` // reuse the last good reading this long when reads fail
	DiscoverTimeout   Duration `json:"discover_timeout"`     // mDNS discovery
	ProbeTimeout      Duration `json:"probe_timeout"`        // "does the page answer" check before casting
	MantelTimeout     Duration `json:"mantel_timeout"`       // reading the phone sleep state from Mantel
	CommandTimeout    Duration `json:"command_timeout"`      // one status/stop request to the device
	LaunchTimeout     Duration `json:"launch_timeout"`       // launching the app and loading the page
}

// Timings are the cast section's timings with defaults filled in.
type Timings struct {
	CheckInterval, CrashIfUpUnder, RelaunchWindow time.Duration
	RelaunchLimit                                 int
	PauseMin, PauseMax, StableAfter               time.Duration
	SleepReportTTL, PhoneKnownFor                 time.Duration
	NightModeRefresh, NightModeStaleFor           time.Duration
	DiscoverTimeout, ProbeTimeout, MantelTimeout  time.Duration
	CommandTimeout, LaunchTimeout                 time.Duration
}

// DefaultTimings are used for every timing left unset.
var DefaultTimings = Timings{
	CheckInterval:     30 * time.Second,
	CrashIfUpUnder:    2 * time.Minute,
	RelaunchWindow:    15 * time.Minute,
	RelaunchLimit:     3,
	PauseMin:          10 * time.Minute,
	PauseMax:          time.Hour,
	StableAfter:       30 * time.Minute,
	SleepReportTTL:    16 * time.Hour,
	PhoneKnownFor:     36 * time.Hour,
	NightModeRefresh:  5 * time.Minute,
	NightModeStaleFor: time.Hour,
	DiscoverTimeout:   4 * time.Second,
	ProbeTimeout:      8 * time.Second,
	MantelTimeout:     8 * time.Second,
	CommandTimeout:    15 * time.Second,
	LaunchTimeout:     30 * time.Second,
}

// Timings resolves the optional timings against DefaultTimings.
func (c *Cast) Timings() Timings {
	d := DefaultTimings
	limit := d.RelaunchLimit
	if c.RelaunchLimit > 0 {
		limit = c.RelaunchLimit
	}
	return Timings{
		CheckInterval:     c.CheckInterval.Or(d.CheckInterval),
		CrashIfUpUnder:    c.CrashIfUpUnder.Or(d.CrashIfUpUnder),
		RelaunchWindow:    c.RelaunchWindow.Or(d.RelaunchWindow),
		RelaunchLimit:     limit,
		PauseMin:          c.PauseMin.Or(d.PauseMin),
		PauseMax:          c.PauseMax.Or(d.PauseMax),
		StableAfter:       c.StableAfter.Or(d.StableAfter),
		SleepReportTTL:    c.SleepReportTTL.Or(d.SleepReportTTL),
		PhoneKnownFor:     c.PhoneKnownFor.Or(d.PhoneKnownFor),
		NightModeRefresh:  c.NightModeRefresh.Or(d.NightModeRefresh),
		NightModeStaleFor: c.NightModeStaleFor.Or(d.NightModeStaleFor),
		DiscoverTimeout:   c.DiscoverTimeout.Or(d.DiscoverTimeout),
		ProbeTimeout:      c.ProbeTimeout.Or(d.ProbeTimeout),
		MantelTimeout:     c.MantelTimeout.Or(d.MantelTimeout),
		CommandTimeout:    c.CommandTimeout.Or(d.CommandTimeout),
		LaunchTimeout:     c.LaunchTimeout.Or(d.LaunchTimeout),
	}
}

// App is the receiver app to keep on screen: AppID, or DashCast when it is
// empty. The one place that default lives; Normalize fills it in too.
func (c *Cast) App() string {
	if c.AppID == "" {
		return AppDashCast
	}
	return c.AppID
}

// DashCast reports whether the app is DashCast.
func (c *Cast) DashCast() bool { return c.App() == AppDashCast }

// IsEnabled reports whether "mantel-cast run" should keep the device (absent = yes).
func (c *Cast) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// Normalize fills defaults and checks the cast section; Mantel Cast uses it on
// its own config too.
func (c *Cast) Normalize() error {
	switch c.Driver {
	case "":
		c.Driver = DriverCast
	case DriverCast, DriverFully:
	default:
		return fmt.Errorf("cast.driver must be %q or %q, not %q", DriverCast, DriverFully, c.Driver)
	}
	if c.Driver == DriverFully {
		if c.AppID != "" && c.AppID != AppDashCast {
			return fmt.Errorf("cast.app_id is for the cast driver, not %q", DriverFully)
		}
		if c.Reload != 0 {
			return fmt.Errorf("cast.reload_seconds is for the cast driver (DashCast); set Fully's own auto reload instead")
		}
	} else if c.Password != "" {
		return fmt.Errorf("cast.password is for the %q driver", DriverFully)
	}
	c.AppID = c.App()
	switch c.Quiet {
	case "":
		c.Quiet = "auto"
	case "auto", "hours", "off":
	default:
		return fmt.Errorf("cast.quiet must be auto, hours or off, not %q", c.Quiet)
	}
	if c.QuietHours != "" {
		start, end, err := conf.ParseWindow(c.QuietHours)
		if err != nil {
			return fmt.Errorf("cast.quiet_hours: %w", err)
		}
		if start == end {
			return fmt.Errorf("cast.quiet_hours %q starts and ends at the same time, so it never applies (all day: \"00:00-24:00\"; never: leave it empty)", c.QuietHours)
		}
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("cast.port %d is not a port", c.Port)
	}
	if c.Reload < 0 {
		return fmt.Errorf("cast.reload_seconds must not be negative (%d)", c.Reload)
	}
	if c.RelaunchLimit < 0 {
		return fmt.Errorf("cast.relaunch_limit must not be negative (%d)", c.RelaunchLimit)
	}
	for _, d := range []struct {
		name string
		v    Duration
	}{
		{"check_interval", c.CheckInterval}, {"crash_if_up_under", c.CrashIfUpUnder},
		{"relaunch_window", c.RelaunchWindow}, {"pause_min", c.PauseMin}, {"pause_max", c.PauseMax},
		{"stable_after", c.StableAfter}, {"sleep_report_ttl", c.SleepReportTTL},
		{"phone_known_for", c.PhoneKnownFor}, {"night_mode_refresh", c.NightModeRefresh},
		{"night_mode_stale_for", c.NightModeStaleFor}, {"discover_timeout", c.DiscoverTimeout},
		{"probe_timeout", c.ProbeTimeout}, {"mantel_timeout", c.MantelTimeout},
		{"command_timeout", c.CommandTimeout}, {"launch_timeout", c.LaunchTimeout},
	} {
		if d.v.Duration < 0 {
			return fmt.Errorf("cast.%s must not be negative (%s)", d.name, d.v.Duration)
		}
	}
	if t := c.Timings(); t.PauseMin > t.PauseMax {
		return fmt.Errorf("cast.pause_min (%s) is longer than cast.pause_max (%s)", t.PauseMin, t.PauseMax)
	}
	return nil
}

// Token is one entry of Mantel's "tokens" list (read when Mantel Cast is given
// Mantel's config.json).
type Token struct {
	Name  string `json:"name"`  // e.g. "nesthub", "iphone"
	Token string `json:"token"` // "$ENV_NAME" reads the environment
	// Scopes beyond viewing: "act" (run actions) and/or "push" (push data).
	Scopes []string `json:"scopes"`
}
