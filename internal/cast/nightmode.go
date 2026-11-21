package cast

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"time"
)

// NightMode is the device's own night mode / do-not-disturb setting, as set
// in the Google Home app (device settings → Night mode). Read-only, from the
// local setup API, which answers this field without a Google login.
type NightMode struct {
	Enabled      bool          `json:"enabled"`
	DoNotDisturb bool          `json:"do_not_disturb"`
	Windows      []NightWindow `json:"windows"`
	Timezone     string        `json:"-"` // the device's zone, from the same reply
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

// nightModeClient is shared so repeated reads can reuse a kept-alive TLS
// connection instead of a fresh handshake each time. The device serves a
// self-signed certificate; this is a LAN read of a setting, nothing secret
// is sent.
var nightModeClient = &http.Client{Transport: &http.Transport{
	TLSClientConfig:     &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // see above
	MaxIdleConnsPerHost: 1,
	IdleConnTimeout:     10 * time.Minute,
	TLSHandshakeTimeout: 5 * time.Second,
}}

// FetchNightMode asks the device at host for its night mode settings
// (within 5 seconds unless ctx ends sooner).
func FetchNightMode(ctx context.Context, host string) (*NightMode, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	u := "https://" + net.JoinHostPort(host, "8443") + "/setup/eureka_info?params=night_mode_params,timezone"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := nightModeClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("eureka_info: HTTP %d", resp.StatusCode)
	}
	n, err := parseNightMode(io.LimitReader(resp.Body, 1<<20))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)) // let the connection be reused
	return n, err
}

func parseNightMode(r io.Reader) (*NightMode, error) {
	var v struct {
		NightMode *NightMode `json:"night_mode_params"`
		Timezone  string     `json:"timezone"`
	}
	if err := json.NewDecoder(r).Decode(&v); err != nil {
		return nil, fmt.Errorf("eureka_info: %w", err)
	}
	if v.NightMode == nil {
		return nil, fmt.Errorf("eureka_info: no night_mode_params (not a display device?)")
	}
	v.NightMode.Timezone = v.Timezone
	return v.NightMode, nil
}

// Active reports whether t falls inside one of the windows (whether or not
// night mode is enabled), and which one.
func (n *NightMode) Active(t time.Time) (bool, NightWindow) {
	for _, w := range n.Windows {
		if w.LengthHours <= 0 {
			continue
		}
		// A window that started on an earlier day may still be running.
		back := int(w.StartHour+w.LengthHours) / 24
		for off := 0; off <= back; off++ {
			d := time.Date(t.Year(), t.Month(), t.Day()-off, 0, 0, 0, 0, t.Location())
			if !hasDay(w.Days, int(d.Weekday())) {
				continue
			}
			// Wall-clock times in t's zone, so a DST change that night does
			// not shift the window by an hour.
			from := int(math.Round(w.StartHour * 60))
			to := from + int(math.Round(w.LengthHours*60))
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

// String is "22:00 起 8 小时" style, for logs.
func (w NightWindow) String() string {
	// Rounded to the minute like Active, so the log shows the real start.
	mins := int(math.Round(w.StartHour * 60))
	return fmt.Sprintf("%02d:%02d+%gh", mins/60, mins%60, w.LengthHours)
}
