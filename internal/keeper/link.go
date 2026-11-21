package keeper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	"github.com/okchum/mantel-cast/internal/config"
)

// Asleep is a phone saying its owner went to sleep (On) or woke up, as
// reported to Mantel (POST /api/v1/quiet) and read back by Mantel Cast.
type Asleep struct {
	On bool      `json:"on"`
	At time.Time `json:"at"`
	By string    `json:"by,omitempty"`
}

// DeviceTokenNames are the token names picked for the device's ?k=, in order
// of preference; otherwise the first token is used.
var DeviceTokenNames = []string{"mantel-cast", "nesthub"}

// PickToken chooses the token the device opens the page with.
func PickToken(tokens []config.Token) string {
	if len(tokens) == 0 {
		return ""
	}
	for _, n := range DeviceTokenNames {
		for _, t := range tokens {
			if t.Name == n {
				return t.Token
			}
		}
	}
	return tokens[0].Token
}

// WithToken adds ?k=<token> when the URL has none. Custom Receivers ignore
// this: their URL (with its own ?k=) lives in the Cast console.
func WithToken(u, token string) string {
	p, err := neturl.Parse(u)
	if token == "" || err != nil || p.Query().Has("k") {
		return u
	}
	q := p.Query()
	q.Set("k", token)
	p.RawQuery = q.Encode()
	return p.String()
}

// Redact hides the access token (the k= value) for logs and errors. A URL
// that does not parse loses its whole query instead.
func Redact(u string) string {
	p, err := neturl.Parse(u)
	if err != nil {
		if i := strings.IndexAny(u, "?#"); i >= 0 {
			return u[:i] + "?…"
		}
		return u
	}
	if !p.Query().Has("k") {
		return u
	}
	q := p.Query()
	q.Set("k", "xxx")
	p.RawQuery = q.Encode()
	return p.String()
}

// FetchAsleep reads GET api/v1/quiet from the Mantel serving page, resolved
// against the page URL the way the page's own fetches are: /index.html and /
// give /api/v1/quiet, a Mantel proxied under /dash/ gives /dash/api/v1/quiet.
// The page's ?k= token goes in a bearer header instead of the URL. ctx bounds
// the request (the keeper gives it cast.mantel_timeout).
func FetchAsleep(ctx context.Context, page string) (*Asleep, error) {
	u, err := neturl.Parse(page)
	if err != nil {
		// url.Error quotes the whole URL, token included.
		var ue *neturl.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("sleep reports: page %s: %w", Redact(page), err)
	}
	tok := u.Query().Get("k")
	u = u.ResolveReference(&neturl.URL{Path: "api/v1/quiet"})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	var v struct {
		Asleep *Asleep `json:"asleep"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&v); err != nil {
		return nil, err
	}
	return v.Asleep, nil
}
