// Package cast is a minimal, dependency-free Google Cast (CastV2) sender:
// mDNS discovery, the TLS/protobuf control channel, receiver status, app
// launch/stop and app-namespace messaging (enough to drive DashCast).
package cast

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	AppDashCast       = "84912283"
	AppBackdrop       = "E8C28D3C"
	NamespaceDashCast = "urn:x-cast:com.madmod.dashcast"
)

const (
	nsConnection = "urn:x-cast:com.google.cast.tp.connection"
	nsHeartbeat  = "urn:x-cast:com.google.cast.tp.heartbeat"
	nsReceiver   = "urn:x-cast:com.google.cast.receiver"

	senderID   = "sender-0"
	receiverID = "receiver-0"

	defaultPort = 8009
)

// ErrClosed is reported by Err (and returned by requests) once Close has been
// called.
var ErrClosed = errors.New("cast: client closed")

// App is one running receiver application as reported by RECEIVER_STATUS.
type App struct {
	AppID, DisplayName, SessionID, TransportID, StatusText string
	IsIdleScreen                                           bool
	Namespaces                                             []string
}

// ReceiverStatus is the decoded RECEIVER_STATUS payload.
type ReceiverStatus struct {
	Apps        []App
	VolumeLevel float64
	Muted       bool
}

// App returns the running app with the given id, or nil.
func (s *ReceiverStatus) App(appID string) *App {
	if s == nil {
		return nil
	}
	for i := range s.Apps {
		if s.Apps[i].AppID == appID {
			return &s.Apps[i]
		}
	}
	return nil
}

type clientConfig struct {
	pingInterval   time.Duration // how often we PING the receiver
	deadAfter      time.Duration // silence after which the connection is considered dead
	requestTimeout time.Duration // default for Status/Stop when ctx has no deadline
	launchTimeout  time.Duration // default for Launch when ctx has no deadline
	writeTimeout   time.Duration // upper bound for a single frame write
}

var defaultConfig = clientConfig{
	pingInterval:   5 * time.Second,
	deadAfter:      15 * time.Second,
	requestTimeout: 10 * time.Second,
	launchTimeout:  30 * time.Second,
	writeTimeout:   10 * time.Second,
}

// Client is a CastV2 control connection to one device. It is safe for
// concurrent use.
type Client struct {
	conn net.Conn
	cfg  clientConfig

	writeMu sync.Mutex
	nextID  atomic.Int64
	// lastRecv is the UnixNano time of the last frame received.
	lastRecv atomic.Int64
	// ponging is set while a PONG is being written, so a device sending
	// PINGs faster than they can be answered gets one pong goroutine, not
	// one per PING.
	ponging atomic.Bool

	mu         sync.Mutex
	pending    map[int]chan inbound
	subs       map[chan *ReceiverStatus]struct{}
	transports map[string]bool // app transports we have CONNECTed to

	done      chan struct{}
	closeOnce sync.Once
	err       error // why the connection died; valid once done is closed
}

// inbound is a JSON control message routed to a waiting request.
type inbound struct {
	typ    string
	reason string
	status *ReceiverStatus // set for RECEIVER_STATUS
}

// Dial opens a TLS connection to a Cast device (port 8009 if port is 0),
// connects to receiver-0 and starts the reader and heartbeat goroutines.
// Devices use self-signed certificates, so the certificate is not verified.
func Dial(ctx context.Context, host string, port int) (*Client, error) {
	return dial(ctx, host, port, defaultConfig)
}

func dial(ctx context.Context, host string, port int, cfg clientConfig) (*Client, error) {
	if port == 0 {
		port = defaultPort
	}
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("cast: dial %s:%d: %w", host, port, err)
	}
	tconn := tls.Client(raw, &tls.Config{
		// Cast devices present self-signed certificates; device
		// authentication (the cast.auth challenge) is not used here.
		InsecureSkipVerify: true, //nolint:gosec // see above
		MinVersion:         tls.VersionTLS12,
	})
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := tconn.HandshakeContext(hctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("cast: tls handshake with %s:%d: %w", host, port, err)
	}
	c := newClient(tconn, cfg)
	if err := c.write(ctx, receiverID, nsConnection, connectPayload()); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func newClient(conn net.Conn, cfg clientConfig) *Client {
	c := &Client{
		conn:       conn,
		cfg:        cfg,
		pending:    make(map[int]chan inbound),
		subs:       make(map[chan *ReceiverStatus]struct{}),
		transports: make(map[string]bool),
		done:       make(chan struct{}),
	}
	c.lastRecv.Store(time.Now().UnixNano())
	go c.readLoop()
	go c.heartbeatLoop()
	return c
}

func connectPayload() map[string]any {
	return map[string]any{
		"type":      "CONNECT",
		"origin":    map[string]any{},
		"userAgent": "mantel",
		"senderInfo": map[string]any{
			"sdkType":        2,
			"version":        "15.605.1.3",
			"browserVersion": "44.0.2403.30",
			"platform":       4,
			"systemVersion":  "Macintosh; Intel Mac OS X10_10_3",
			"connectionType": 1,
		},
	}
}

// Close sends a best-effort CLOSE to the receiver and closes the connection.
func (c *Client) Close() error {
	select {
	case <-c.done:
		return nil
	default:
	}
	// Best effort, bounded by a short write deadline, and skipped when
	// another write holds writeMu (it may be stuck for up to writeTimeout):
	// closing the connection below unblocks that write, so Close returns
	// promptly either way.
	msg := &castMessage{SourceID: senderID, DestinationID: receiverID, Namespace: nsConnection, PayloadUTF8: `{"type":"CLOSE"}`}
	if c.writeMu.TryLock() {
		_ = c.conn.SetWriteDeadline(time.Now().Add(time.Second))
		_ = writeFrame(c.conn, msg)
		c.writeMu.Unlock()
	}
	c.shutdown(ErrClosed)
	return nil
}

// Done is closed when the connection dies (or Close is called).
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports why the connection died. It returns nil while the connection
// is alive.
func (c *Client) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

func (c *Client) shutdown(err error) {
	c.closeOnce.Do(func() {
		c.err = err
		_ = c.conn.Close()
		close(c.done)
	})
}

// write sends one JSON (or raw string) payload. payload may be a string,
// []byte, json.RawMessage, or anything json.Marshal accepts.
func (c *Client) write(ctx context.Context, dest, namespace string, payload any) error {
	var body string
	switch p := payload.(type) {
	case string:
		body = p
	case []byte:
		body = string(p)
	case json.RawMessage:
		body = string(p)
	default:
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("cast: encode payload: %w", err)
		}
		body = string(b)
	}
	msg := &castMessage{
		SourceID:      senderID,
		DestinationID: dest,
		Namespace:     namespace,
		PayloadType:   payloadString,
		PayloadUTF8:   body,
	}
	return c.writeMsg(ctx, msg)
}

func (c *Client) writeMsg(ctx context.Context, msg *castMessage) error {
	if err := c.Err(); err != nil {
		return fmt.Errorf("cast: connection closed: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(c.cfg.writeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	// Encode first: a message that is too large is this request's problem,
	// not the connection's.
	frame, err := encodeFrame(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(deadline)
	if _, err := c.conn.Write(frame); err != nil {
		// A partial frame desynchronises the stream; the connection is unusable.
		c.shutdown(fmt.Errorf("cast: write: %w", err))
		return fmt.Errorf("cast: write: %w", err)
	}
	return nil
}

func (c *Client) readLoop() {
	for {
		msg, err := readFrame(c.conn)
		if err != nil {
			c.shutdown(fmt.Errorf("cast: read: %w", err))
			return
		}
		c.lastRecv.Store(time.Now().UnixNano())
		c.handle(msg)
	}
}

func (c *Client) heartbeatLoop() {
	t := time.NewTicker(c.cfg.pingInterval)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			silent := time.Since(time.Unix(0, c.lastRecv.Load()))
			if silent > c.cfg.deadAfter {
				c.shutdown(fmt.Errorf("cast: no data from device for %s", silent.Round(time.Millisecond)))
				return
			}
			_ = c.write(context.Background(), receiverID, nsHeartbeat, `{"type":"PING"}`)
		}
	}
}

type rawApp struct {
	AppID        string `json:"appId"`
	DisplayName  string `json:"displayName"`
	SessionID    string `json:"sessionId"`
	TransportID  string `json:"transportId"`
	StatusText   string `json:"statusText"`
	IsIdleScreen bool   `json:"isIdleScreen"`
	Namespaces   []struct {
		Name string `json:"name"`
	} `json:"namespaces"`
}

type rawReceiverStatus struct {
	Status struct {
		Applications []rawApp `json:"applications"`
		Volume       struct {
			Level float64 `json:"level"`
			Muted bool    `json:"muted"`
		} `json:"volume"`
	} `json:"status"`
}

func parseReceiverStatus(payload []byte) (*ReceiverStatus, error) {
	var raw rawReceiverStatus
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, err
	}
	st := &ReceiverStatus{
		VolumeLevel: raw.Status.Volume.Level,
		Muted:       raw.Status.Volume.Muted,
	}
	for _, a := range raw.Status.Applications {
		app := App{
			AppID:        a.AppID,
			DisplayName:  a.DisplayName,
			SessionID:    a.SessionID,
			TransportID:  a.TransportID,
			StatusText:   a.StatusText,
			IsIdleScreen: a.IsIdleScreen,
		}
		for _, ns := range a.Namespaces {
			app.Namespaces = append(app.Namespaces, ns.Name)
		}
		st.Apps = append(st.Apps, app)
	}
	return st, nil
}

func (c *Client) handle(msg *castMessage) {
	if msg.PayloadType != payloadString {
		return
	}
	var hdr struct {
		Type      string `json:"type"`
		RequestID int    `json:"requestId"`
		Reason    string `json:"reason"`
	}
	payload := []byte(msg.PayloadUTF8)
	if err := json.Unmarshal(payload, &hdr); err != nil {
		return
	}
	switch msg.Namespace {
	case nsHeartbeat:
		if hdr.Type == "PING" {
			pong := &castMessage{
				SourceID:      msg.DestinationID,
				DestinationID: msg.SourceID,
				Namespace:     nsHeartbeat,
				PayloadUTF8:   `{"type":"PONG"}`,
			}
			if c.ponging.CompareAndSwap(false, true) {
				go func() {
					defer c.ponging.Store(false)
					_ = c.writeMsg(context.Background(), pong)
				}()
			}
		}
		return
	case nsConnection:
		if hdr.Type == "CLOSE" {
			if msg.SourceID == receiverID {
				c.shutdown(errors.New("cast: receiver closed the connection"))
				return
			}
			c.mu.Lock()
			delete(c.transports, msg.SourceID)
			c.mu.Unlock()
		}
		return
	}

	in := inbound{typ: hdr.Type, reason: hdr.Reason}
	if msg.Namespace == nsReceiver && hdr.Type == "RECEIVER_STATUS" {
		st, err := parseReceiverStatus(payload)
		if err != nil {
			return
		}
		in.status = st
		c.mu.Lock()
		for ch := range c.subs {
			select {
			case ch <- st:
			default:
			}
		}
		c.mu.Unlock()
	}
	if hdr.RequestID == 0 {
		return
	}
	c.mu.Lock()
	ch := c.pending[hdr.RequestID]
	c.mu.Unlock()
	if ch != nil {
		select {
		case ch <- in:
		default:
		}
	}
}

// register allocates a request id and a channel receiving responses to it.
func (c *Client) register() (int, chan inbound, func()) {
	id := int(c.nextID.Add(1))
	ch := make(chan inbound, 4)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	return id, ch, func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}
}

func (c *Client) subscribe() (chan *ReceiverStatus, func()) {
	ch := make(chan *ReceiverStatus, 8)
	c.mu.Lock()
	c.subs[ch] = struct{}{}
	c.mu.Unlock()
	return ch, func() {
		c.mu.Lock()
		delete(c.subs, ch)
		c.mu.Unlock()
	}
}

func withDefaultTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

func (c *Client) deadErr() error {
	return fmt.Errorf("cast: connection closed: %w", c.err)
}

// ResponseError is the device refusing a request (LAUNCH_ERROR,
// INVALID_REQUEST, ...): the app-level failure of one command, while the
// connection itself is fine.
type ResponseError struct {
	Op, Type, Reason string
}

func (e *ResponseError) Error() string {
	reason := e.Reason
	if reason == "" {
		reason = "unknown reason"
	}
	return fmt.Sprintf("cast: %s failed: %s: %s", e.Op, e.Type, reason)
}

func responseError(op string, in inbound) error {
	return &ResponseError{Op: op, Type: in.typ, Reason: in.reason}
}

func isErrorType(t string) bool {
	switch t {
	case "LAUNCH_ERROR", "INVALID_REQUEST", "LOAD_FAILED", "LOAD_CANCELLED", "INVALID_PLAYER_STATE":
		return true
	}
	return false
}

// receiverRequest sends payload (with a fresh requestId) to receiver-0 and
// waits for a RECEIVER_STATUS answer to it.
func (c *Client) receiverRequest(ctx context.Context, op string, payload map[string]any) (*ReceiverStatus, error) {
	ctx, cancel := withDefaultTimeout(ctx, c.cfg.requestTimeout)
	defer cancel()
	id, ch, unregister := c.register()
	defer unregister()
	payload["requestId"] = id
	if err := c.write(ctx, receiverID, nsReceiver, payload); err != nil {
		return nil, err
	}
	for {
		select {
		case in := <-ch:
			if isErrorType(in.typ) {
				return nil, responseError(op, in)
			}
			if in.status != nil {
				return in.status, nil
			}
		case <-c.done:
			return nil, c.deadErr()
		case <-ctx.Done():
			return nil, fmt.Errorf("cast: %s: %w", op, ctx.Err())
		}
	}
}

// Status asks the receiver for its current status (GET_STATUS).
func (c *Client) Status(ctx context.Context) (*ReceiverStatus, error) {
	return c.receiverRequest(ctx, "GET_STATUS", map[string]any{"type": "GET_STATUS"})
}

// Stop stops the app session with the given id (STOP).
func (c *Client) Stop(ctx context.Context, sessionID string) error {
	_, err := c.receiverRequest(ctx, "STOP", map[string]any{"type": "STOP", "sessionId": sessionID})
	return err
}

// Launch launches appID and waits until a RECEIVER_STATUS reports it running
// with a transportId.
func (c *Client) Launch(ctx context.Context, appID string) (*App, error) {
	ctx, cancel := withDefaultTimeout(ctx, c.cfg.launchTimeout)
	defer cancel()
	statuses, unsubscribe := c.subscribe()
	defer unsubscribe()
	id, ch, unregister := c.register()
	defer unregister()
	payload := map[string]any{"type": "LAUNCH", "appId": appID, "requestId": id}
	if err := c.write(ctx, receiverID, nsReceiver, payload); err != nil {
		return nil, err
	}
	check := func(st *ReceiverStatus) *App {
		if a := st.App(appID); a != nil && a.TransportID != "" {
			cp := *a
			cp.Namespaces = append([]string(nil), a.Namespaces...)
			return &cp
		}
		return nil
	}
	for {
		select {
		case in := <-ch:
			if isErrorType(in.typ) {
				return nil, responseError("LAUNCH "+appID, in)
			}
			if in.status != nil {
				if a := check(in.status); a != nil {
					return a, nil
				}
			}
		case st := <-statuses:
			if a := check(st); a != nil {
				return a, nil
			}
		case <-c.done:
			return nil, c.deadErr()
		case <-ctx.Done():
			return nil, fmt.Errorf("cast: LAUNCH %s: %w", appID, ctx.Err())
		}
	}
}

// Send sends a JSON payload on namespace to an app transport, issuing a
// virtual-connection CONNECT to that transport first if needed.
func (c *Client) Send(ctx context.Context, transportID, namespace string, payload any) error {
	if transportID == "" {
		return errors.New("cast: empty transport id")
	}
	c.mu.Lock()
	connected := c.transports[transportID]
	c.mu.Unlock()
	if !connected {
		if err := c.write(ctx, transportID, nsConnection, connectPayload()); err != nil {
			return err
		}
		c.mu.Lock()
		c.transports[transportID] = true
		c.mu.Unlock()
	}
	return c.write(ctx, transportID, namespace, payload)
}

// LoadURL makes the device display url via the DashCast receiver, launching
// DashCast first unless it is already running. force asks DashCast to load
// the page even if it reports X-Frame-Options restrictions; reloadSeconds > 0
// makes DashCast reload the page periodically. It returns the DashCast
// session the page was sent to.
func (c *Client) LoadURL(ctx context.Context, url string, force bool, reloadSeconds int) (*App, error) {
	st, err := c.Status(ctx)
	if err != nil {
		return nil, err
	}
	app := st.App(AppDashCast)
	if app == nil || app.TransportID == "" {
		if app, err = c.Launch(ctx, AppDashCast); err != nil {
			return nil, err
		}
	}
	if err := c.Send(ctx, app.TransportID, NamespaceDashCast, loadPayload(url, force, reloadSeconds)); err != nil {
		return nil, err
	}
	return app, nil
}

// loadPayload is DashCast's load message.
func loadPayload(url string, force bool, reloadSeconds int) map[string]any {
	return map[string]any{
		"url":         url,
		"force":       force,
		"reload":      reloadSeconds != 0,
		"reload_time": reloadSeconds,
	}
}
