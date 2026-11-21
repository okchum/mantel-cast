package keeper

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/okchum/mantel-cast/internal/cast"
	"github.com/okchum/mantel-cast/internal/config"
	"github.com/okchum/mantel-kit/conf"
)

// QuietInputs are the signals Decide weighs.
type QuietInputs struct {
	Phone *Asleep         // nil: no phone reports
	Hub   *cast.NightMode // nil: unknown
	Hours string          // cast.quiet_hours
	Loc   *time.Location  // zone for Hours (and Hub windows without a zone)
	// A sleep report counts for SleepTTL: a phone that never says "awake"
	// (flat battery, automation deleted) must not keep the screen dark.
	// Reports newer than PhoneKnownFor mean "a phone is reporting": its
	// "awake" then overrides the schedules (stayed up late, weekend lie-in).
	// Zero = cast.sleep_report_ttl / phone_known_for defaults.
	SleepTTL, PhoneKnownFor time.Duration
}

// Decide says whether the screen should be left alone now, and why.
// Order: Hub do-not-disturb (said out loud, always wins) → phone sleep
// report → Hub night mode schedule → quiet_hours.
func Decide(mode string, in QuietInputs, now time.Time) (bool, string) {
	loc := in.Loc
	if loc == nil {
		loc = time.Local
	}
	sleepTTL, known := in.SleepTTL, in.PhoneKnownFor
	if sleepTTL <= 0 {
		sleepTTL = config.DefaultTimings.SleepReportTTL
	}
	if known <= 0 {
		known = config.DefaultTimings.PhoneKnownFor
	}
	hours := func() (bool, string) {
		if in.Hours != "" && conf.InWindow(in.Hours, now.In(loc)) {
			return true, "quiet hours " + in.Hours
		}
		return false, ""
	}
	switch mode {
	case "off":
		return false, ""
	case "hours":
		return hours()
	}
	if in.Hub != nil && in.Hub.DoNotDisturb {
		return true, "do not disturb on the device"
	}
	if p := in.Phone; p != nil && now.Sub(p.At) < known {
		if p.On && now.Sub(p.At) < sleepTTL {
			return true, "phone asleep since " + p.At.In(loc).Format("15:04")
		}
		if !p.On {
			return false, ""
		}
	}
	if h := in.Hub; h != nil && h.Enabled {
		hl := loc
		if l := zone(h.Timezone); l != nil {
			hl = l
		}
		if ok, w := h.Active(now.In(hl)); ok {
			return true, "night mode on the device " + w.String()
		}
	}
	return hours()
}

// zones caches time.LoadLocation by name (nil = unknown zone): the device
// reports the same zone every check, and loading it reads tzdata each time.
var zones sync.Map // string → *time.Location

// zone is the named location, or nil for "" or an unknown name (logged once).
func zone(name string) *time.Location {
	if name == "" {
		return nil
	}
	if l, ok := zones.Load(name); ok {
		return l.(*time.Location)
	}
	l, err := time.LoadLocation(name)
	if err != nil {
		l = nil
	}
	if _, loaded := zones.LoadOrStore(name, l); !loaded && l == nil {
		// Cached, so this is logged once per name.
		logf("keeper: the device reports time zone %q, which is unknown (%v); its night mode is read in the configured zone", name, err)
	}
	return l
}

func (k *Keeper) quietNow(ctx context.Context, now time.Time) (bool, string) {
	t := k.Cfg.Timings()
	in := QuietInputs{Hours: k.Cfg.QuietHours, Loc: k.Loc, SleepTTL: t.SleepReportTTL, PhoneKnownFor: t.PhoneKnownFor}
	mode := k.Cfg.Quiet
	if mode == "" {
		mode = "auto"
	}
	if mode == "auto" {
		// The two reads are independent: run them side by side.
		var (
			wg    sync.WaitGroup
			phone *Asleep
			perr  error
		)
		if k.Phone != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				pctx, cancel := context.WithTimeout(ctx, t.MantelTimeout)
				defer cancel()
				phone, perr = k.Phone(pctx)
			}()
		}
		in.Hub = k.hubNight(ctx, now)
		wg.Wait()
		if k.Phone != nil {
			if perr == nil {
				in.Phone = phone
				if k.phoneErr != "" {
					k.phoneErr = ""
					logf("quiet: phone sleep state readable again")
				}
			} else if k.phoneErr != perr.Error() {
				k.phoneErr = perr.Error()
				logf("quiet: cannot read phone sleep state: %v", perr)
			}
		}
	}
	return Decide(mode, in, now)
}

// hubNight reads the device's night mode, at most every night_mode_refresh,
// falling back to the last good reading for night_mode_stale_for so a
// network blip at 2am does not end the night.
func (k *Keeper) hubNight(ctx context.Context, now time.Time) *cast.NightMode {
	if k.host == "" {
		return nil
	}
	t := k.Cfg.Timings()
	if k.hub != nil && now.Sub(k.hubAt) < t.NightModeRefresh {
		return k.hub
	}
	fetch := k.NightMode
	if fetch == nil {
		fetch = cast.FetchNightMode
	}
	n, err := fetch(ctx, k.host)
	if err == nil {
		k.hub, k.hubAt = n, now
		if k.hubErr != "" {
			k.hubErr = ""
			logf("quiet: the device's night mode is readable again")
		}
		return n
	}
	if msg := fmt.Sprint(err); msg != k.hubErr {
		k.hubErr = msg
		logf("quiet: cannot read the device's night mode: %v", err)
	}
	if k.hub != nil && now.Sub(k.hubAt) < t.NightModeStaleFor {
		return k.hub
	}
	return nil
}
