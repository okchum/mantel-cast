package cast

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/okchum/mantel-cast/internal/display"
)

// The device's local setup API (https on 8443) answers a few eureka_info
// fields without a Google login: the device name, its UUID, the time zone and
// the night mode settings made in the Google Home app.

// setupClient is shared so repeated reads can reuse a kept-alive TLS
// connection instead of a fresh handshake each time. The device serves a
// self-signed certificate; this is a LAN read of settings, nothing secret
// is sent.
var setupClient = &http.Client{Transport: &http.Transport{
	TLSClientConfig:     &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // see above
	MaxIdleConnsPerHost: 1,
	IdleConnTimeout:     10 * time.Minute,
	TLSHandshakeTimeout: 5 * time.Second,
}}

// setupPort is where eureka_info is served; a var for tests.
var setupPort = "8443"

// eurekaInfo GETs /setup/eureka_info?params=… from the device at host
// (within 5 seconds unless ctx ends sooner) and decodes it into v.
func eurekaInfo(ctx context.Context, host, params string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	u := "https://" + net.JoinHostPort(host, setupPort) + "/setup/eureka_info?params=" + params
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := setupClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("eureka_info: HTTP %d", resp.StatusCode)
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)) // let the connection be reused
	if err != nil {
		return fmt.Errorf("eureka_info: %w", err)
	}
	return nil
}

// FetchNightMode asks the device at host for its night mode settings.
func FetchNightMode(ctx context.Context, host string) (*display.NightMode, error) {
	var v nightReply
	if err := eurekaInfo(ctx, host, "night_mode_params,timezone", &v); err != nil {
		return nil, err
	}
	return v.nightMode()
}

type nightReply struct {
	NightMode *display.NightMode `json:"night_mode_params"`
	Timezone  string             `json:"timezone"`
}

func (v nightReply) nightMode() (*display.NightMode, error) {
	if v.NightMode == nil {
		return nil, fmt.Errorf("eureka_info: no night_mode_params (not a display device?)")
	}
	v.NightMode.Timezone = v.Timezone
	return v.NightMode, nil
}

func parseNightMode(r io.Reader) (*display.NightMode, error) {
	var v nightReply
	if err := json.NewDecoder(r).Decode(&v); err != nil {
		return nil, fmt.Errorf("eureka_info: %w", err)
	}
	return v.nightMode()
}

// Identify asks the device at host for its name and UUID (the UUID without
// dashes, as mDNS discovery reports it).
func Identify(ctx context.Context, host string) (display.Device, error) {
	var v struct {
		Name string `json:"name"`
		UDN  string `json:"ssdp_udn"`
	}
	if err := eurekaInfo(ctx, host, "name,ssdp_udn", &v); err != nil {
		return display.Device{}, err
	}
	if v.Name == "" && v.UDN == "" {
		return display.Device{}, fmt.Errorf("eureka_info: no name or ssdp_udn")
	}
	return display.Device{Name: v.Name, ID: strings.ReplaceAll(strings.ToLower(v.UDN), "-", ""), Host: host}, nil
}
