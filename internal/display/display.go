// Package display is what the keeper needs from a screen technology, kept
// apart from any one of them: Google Cast (Nest Hub, Chromecast) is one
// driver, Fully Kiosk Browser on an Android tablet another. A driver finds
// devices, connects to one, tells what it shows, puts a page on it and takes
// it off again; the keeper decides when.
package display

import (
	"context"
	"errors"
	"time"
)

// Driver is one screen technology.
type Driver interface {
	// Name is the config value that selects the driver ("cast", "fully").
	Name() string
	// LoadsPage reports whether Show opens the URL it is given (DashCast,
	// a kiosk browser), as opposed to an app that shows its own page (a
	// Cast Custom Receiver). Only then is the page probed before showing.
	LoadsPage() bool
	// Discover lists the devices on the LAN; drivers without discovery
	// return ErrNoDiscovery (the host must then be configured).
	Discover(ctx context.Context, timeout time.Duration) ([]Device, error)
	// Dial connects to the device at host (port 0 = the driver's default).
	Dial(ctx context.Context, host string, port int) (Session, error)
}

// Device is a device a driver found or identified.
type Device struct {
	Name, Model string
	ID          string // stable identity (Cast UUID, Fully device id); "" = unknown
	Host        string
	Port        int
}

// Session is a live connection to one device.
type Session interface {
	// Observe reports what the device shows now.
	Observe(ctx context.Context) (Screen, error)
	// Show puts url on screen (drivers that do not load a page ignore it)
	// and returns the handle of what now shows it.
	Show(ctx context.Context, url string) (handle string, err error)
	// Hide takes what handle names off the screen, handing it back to the
	// device's own idle or night screen.
	Hide(ctx context.Context, handle string) error
	// Done is closed when the connection is lost.
	Done() <-chan struct{}
	Close() error
}

// Screen is what a device shows.
type Screen struct {
	// Showing names what is on screen for logs ("YouTube"); "" = nothing.
	Showing string
	// Handle identifies the instance on screen (a Cast session id), so the
	// keeper can tell the one it started from the same app started by
	// someone else.
	Handle string
	// Ours: the configured app or page is on screen, whoever put it there.
	Ours bool
	// Idle: nothing, or the device's own idle / ambient / screen-off state,
	// which the dashboard may replace without taking the screen from anyone.
	Idle bool
	// Detail is one line per thing running, for "mantel-cast status".
	Detail []string
}

// ErrRefused is matched (errors.Is) by errors of a device that answered but
// refused a command: the connection is fine and the command is retried on
// the next check instead of reconnecting.
var ErrRefused = errors.New("device refused the command")

// ErrNoDiscovery is returned by Discover of drivers that cannot find
// devices by themselves.
var ErrNoDiscovery = errors.New("this device type cannot be discovered; set its host")

// Identifier is implemented by sessions that can tell which device answered,
// so a reconnect to a remembered address notices a different device there
// (DHCP gave the address to someone else).
type Identifier interface {
	Identify(ctx context.Context) (Device, error)
}

// NightModer is implemented by sessions whose device has its own night mode
// or do-not-disturb setting, one of the quiet signals.
type NightModer interface {
	NightMode(ctx context.Context) (*NightMode, error)
}
