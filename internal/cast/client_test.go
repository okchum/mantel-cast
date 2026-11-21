package cast

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fake-cast-device"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

type fakeApp struct {
	id, name, session string
	idle              bool
}

// fakeDevice is a minimal CastV2 receiver over a real local TLS listener.
type fakeDevice struct {
	t  *testing.T
	ln net.Listener

	silent atomic.Bool // when true, receive but never answer

	mu       sync.Mutex
	conn     net.Conn
	received []*castMessage
	apps     []fakeApp
	sessions int
	writeMu  sync.Mutex
}

func newFakeDevice(t *testing.T) *fakeDevice {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDevice{t: t, ln: ln, apps: []fakeApp{{id: AppBackdrop, name: "Backdrop", session: "backdrop-session", idle: true}}}
	t.Cleanup(func() { _ = ln.Close() })
	go f.serve()
	return f
}

func (f *fakeDevice) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeDevice) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conn = conn
		f.mu.Unlock()
		go f.handleConn(conn)
	}
}

func (f *fakeDevice) send(conn net.Conn, src, dst, ns string, payload any) {
	b, _ := json.Marshal(payload)
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	_ = writeFrame(conn, &castMessage{SourceID: src, DestinationID: dst, Namespace: ns, PayloadUTF8: string(b)})
}

// toClient sends a message on the current connection.
func (f *fakeDevice) toClient(src, ns string, payload any) {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	f.send(conn, src, senderID, ns, payload)
}

func (f *fakeDevice) statusPayload(reqID int, withTransport bool) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	apps := []map[string]any{}
	for _, a := range f.apps {
		m := map[string]any{
			"appId": a.id, "displayName": a.name, "sessionId": a.session,
			"isIdleScreen": a.idle, "statusText": a.name + " running",
			"namespaces": []map[string]string{{"name": NamespaceDashCast}},
		}
		if withTransport {
			m["transportId"] = a.session
		}
		apps = append(apps, m)
	}
	return map[string]any{
		"type": "RECEIVER_STATUS", "requestId": reqID,
		"status": map[string]any{
			"applications": apps,
			"volume":       map[string]any{"level": 0.25, "muted": true},
		},
	}
}

func (f *fakeDevice) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	for {
		msg, err := readFrame(conn)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.received = append(f.received, msg)
		f.mu.Unlock()
		if f.silent.Load() {
			continue
		}
		var req struct {
			Type      string `json:"type"`
			RequestID int    `json:"requestId"`
			AppID     string `json:"appId"`
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal([]byte(msg.PayloadUTF8), &req)
		reply := func(p any) { f.send(conn, msg.DestinationID, msg.SourceID, msg.Namespace, p) }
		switch {
		case msg.Namespace == nsHeartbeat && req.Type == "PING":
			reply(map[string]string{"type": "PONG"})
		case msg.Namespace == nsReceiver && req.Type == "GET_STATUS":
			reply(f.statusPayload(req.RequestID, true))
		case msg.Namespace == nsReceiver && req.Type == "LAUNCH":
			switch req.AppID {
			case "NOTFOUND":
				reply(map[string]any{"type": "LAUNCH_ERROR", "reason": "NOT_FOUND", "requestId": req.RequestID})
				continue
			case "INVALID":
				reply(map[string]any{"type": "INVALID_REQUEST", "reason": "INVALID_COMMAND", "requestId": req.RequestID})
				continue
			}
			f.mu.Lock()
			f.sessions++
			f.apps = []fakeApp{{id: req.AppID, name: "App " + req.AppID, session: fmt.Sprintf("session-%d", f.sessions)}}
			f.mu.Unlock()
			// Like real devices, report the app first while it is still
			// starting (no transport yet), then broadcast the final status.
			reply(f.statusPayload(req.RequestID, false))
			time.Sleep(20 * time.Millisecond)
			reply(f.statusPayload(0, true))
		case msg.Namespace == nsReceiver && req.Type == "STOP":
			f.mu.Lock()
			var kept []fakeApp
			for _, a := range f.apps {
				if a.session != req.SessionID {
					kept = append(kept, a)
				}
			}
			f.apps = kept
			f.mu.Unlock()
			reply(f.statusPayload(req.RequestID, true))
		}
	}
}

// messages returns received messages matching ns and (optionally) type.
func (f *fakeDevice) messages(ns, typ string) []*castMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*castMessage
	for _, m := range f.received {
		if m.Namespace != ns {
			continue
		}
		if typ != "" {
			var h struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(m.PayloadUTF8), &h)
			if h.Type != typ {
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func testDial(t *testing.T, f *fakeDevice, cfg clientConfig) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dial(ctx, "127.0.0.1", f.port(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestClientConnectStatusLaunchLoadURL(t *testing.T) {
	f := newFakeDevice(t)
	c := testDial(t, f, defaultConfig)
	ctx := testCtx(t)

	waitFor(t, "CONNECT to receiver-0", func() bool { return len(f.messages(nsConnection, "CONNECT")) == 1 })
	conn := f.messages(nsConnection, "CONNECT")[0]
	if conn.SourceID != senderID || conn.DestinationID != receiverID {
		t.Fatalf("CONNECT %s -> %s", conn.SourceID, conn.DestinationID)
	}

	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Apps) != 1 || st.Apps[0].AppID != AppBackdrop || !st.Apps[0].IsIdleScreen ||
		st.Apps[0].TransportID != "backdrop-session" || st.Apps[0].StatusText != "Backdrop running" ||
		len(st.Apps[0].Namespaces) != 1 || st.VolumeLevel != 0.25 || !st.Muted {
		t.Fatalf("unexpected status %+v", st)
	}

	// Backdrop is showing: LoadURL must launch DashCast, connect to its
	// transport and send the load message.
	app, err := c.LoadURL(ctx, "http://dash.local/", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if app.AppID != AppDashCast || app.SessionID != "session-1" {
		t.Fatalf("LoadURL app %+v", app)
	}
	if n := len(f.messages(nsReceiver, "LAUNCH")); n != 1 {
		t.Fatalf("LAUNCH sent %d times", n)
	}
	waitFor(t, "dashcast message", func() bool { return len(f.messages(NamespaceDashCast, "")) == 1 })
	connects := f.messages(nsConnection, "CONNECT")
	if len(connects) != 2 || connects[1].DestinationID != "session-1" {
		t.Fatalf("expected CONNECT to session-1, got %d connects", len(connects))
	}
	dm := f.messages(NamespaceDashCast, "")[0]
	if dm.DestinationID != "session-1" || dm.SourceID != senderID {
		t.Fatalf("dashcast message routed %s -> %s", dm.SourceID, dm.DestinationID)
	}
	var load map[string]any
	if err := json.Unmarshal([]byte(dm.PayloadUTF8), &load); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"url": "http://dash.local/", "force": true, "reload": false, "reload_time": float64(0)}
	if fmt.Sprint(load) != fmt.Sprint(want) {
		t.Fatalf("load payload %v, want %v", load, want)
	}

	// DashCast already running: no new LAUNCH, no second CONNECT.
	if _, err := c.LoadURL(ctx, "http://dash.local/2", false, 60); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second dashcast message", func() bool { return len(f.messages(NamespaceDashCast, "")) == 2 })
	if n := len(f.messages(nsReceiver, "LAUNCH")); n != 1 {
		t.Fatalf("LAUNCH sent %d times", n)
	}
	if n := len(f.messages(nsConnection, "CONNECT")); n != 2 {
		t.Fatalf("CONNECT sent %d times", n)
	}
	_ = json.Unmarshal([]byte(f.messages(NamespaceDashCast, "")[1].PayloadUTF8), &load)
	if load["reload"] != true || load["reload_time"] != float64(60) || load["force"] != false {
		t.Fatalf("reload payload %v", load)
	}

	// The app closes its virtual connection: the next Send reconnects.
	f.toClient("session-1", nsConnection, map[string]string{"type": "CLOSE"})
	waitFor(t, "transport forgotten", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return !c.transports["session-1"]
	})
	if err := c.Send(ctx, "session-1", NamespaceDashCast, map[string]string{"url": "x"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "re-CONNECT", func() bool { return len(f.messages(nsConnection, "CONNECT")) == 3 })

	if err := c.Stop(ctx, "session-1"); err != nil {
		t.Fatal(err)
	}
	st, err = c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Apps) != 0 {
		t.Fatalf("apps after STOP: %+v", st.Apps)
	}
	select {
	case <-c.Done():
		t.Fatal("connection died unexpectedly")
	default:
	}
}

func TestLaunchErrors(t *testing.T) {
	f := newFakeDevice(t)
	c := testDial(t, f, defaultConfig)
	ctx := testCtx(t)
	for app, reason := range map[string]string{"NOTFOUND": "NOT_FOUND", "INVALID": "INVALID_COMMAND"} {
		_, err := c.Launch(ctx, app)
		if err == nil || !strings.Contains(err.Error(), reason) {
			t.Fatalf("Launch(%s) error = %v, want reason %s", app, err, reason)
		}
	}
	a, err := c.Launch(ctx, "ABCD1234")
	if err != nil {
		t.Fatal(err)
	}
	if a.AppID != "ABCD1234" || a.TransportID == "" || a.SessionID == "" {
		t.Fatalf("launched app %+v", a)
	}
}

func TestRequestTimeout(t *testing.T) {
	f := newFakeDevice(t)
	c := testDial(t, f, defaultConfig)
	f.silent.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Status(ctx); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("Status error = %v", err)
	}
}

func TestHeartbeat(t *testing.T) {
	f := newFakeDevice(t)
	cfg := defaultConfig
	cfg.pingInterval = 20 * time.Millisecond
	c := testDial(t, f, cfg)

	waitFor(t, "client PINGs", func() bool { return len(f.messages(nsHeartbeat, "PING")) >= 3 })
	ping := f.messages(nsHeartbeat, "PING")[0]
	if ping.SourceID != senderID || ping.DestinationID != receiverID {
		t.Fatalf("PING routed %s -> %s", ping.SourceID, ping.DestinationID)
	}

	f.toClient(receiverID, nsHeartbeat, map[string]string{"type": "PING"})
	waitFor(t, "client PONG", func() bool { return len(f.messages(nsHeartbeat, "PONG")) == 1 })
	pong := f.messages(nsHeartbeat, "PONG")[0]
	if pong.SourceID != senderID || pong.DestinationID != receiverID {
		t.Fatalf("PONG routed %s -> %s", pong.SourceID, pong.DestinationID)
	}

	// The fake answers PINGs, so the connection stays up well past deadAfter.
	time.Sleep(100 * time.Millisecond)
	if err := c.Err(); err != nil {
		t.Fatalf("connection died: %v", err)
	}
}

func TestDeadConnectionDetected(t *testing.T) {
	f := newFakeDevice(t)
	cfg := defaultConfig
	cfg.pingInterval = 20 * time.Millisecond
	cfg.deadAfter = 100 * time.Millisecond
	f.silent.Store(true)
	c := testDial(t, f, cfg)
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("silent connection not declared dead")
	}
	if err := c.Err(); err == nil || !strings.Contains(err.Error(), "no data") {
		t.Fatalf("Err = %v", err)
	}
	if _, err := c.Status(testCtx(t)); err == nil {
		t.Fatal("Status succeeded on dead connection")
	}
}

func TestPendingRequestFailsWhenConnectionDrops(t *testing.T) {
	f := newFakeDevice(t)
	c := testDial(t, f, defaultConfig)
	f.silent.Store(true)
	errc := make(chan error, 1)
	go func() { _, err := c.Launch(testCtx(t), AppDashCast); errc <- err }()
	waitFor(t, "LAUNCH sent", func() bool { return len(f.messages(nsReceiver, "LAUNCH")) == 1 })
	f.toClient(receiverID, nsConnection, map[string]string{"type": "CLOSE"})
	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "receiver closed") {
			t.Fatalf("Launch error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Launch did not fail after receiver CLOSE")
	}
	<-c.Done()
}

func TestServerHangup(t *testing.T) {
	f := newFakeDevice(t)
	c := testDial(t, f, defaultConfig)
	waitFor(t, "connection", func() bool { return len(f.messages(nsConnection, "CONNECT")) == 1 })
	f.mu.Lock()
	_ = f.conn.Close()
	f.mu.Unlock()
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("hangup not detected")
	}
	if c.Err() == nil {
		t.Fatal("Err is nil after hangup")
	}
}

func TestClose(t *testing.T) {
	f := newFakeDevice(t)
	c := testDial(t, f, defaultConfig)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	<-c.Done()
	if c.Err() != ErrClosed {
		t.Fatalf("Err = %v", c.Err())
	}
	waitFor(t, "CLOSE at device", func() bool { return len(f.messages(nsConnection, "CLOSE")) == 1 })
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := c.Send(testCtx(t), "t", NamespaceDashCast, "{}"); err == nil {
		t.Fatal("Send after Close succeeded")
	}
}

func TestDialErrors(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	// A plain TCP server that hangs up immediately: the TLS handshake fails.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	if _, err := Dial(testCtx(t), "127.0.0.1", port); err == nil {
		t.Fatal("Dial succeeded against non-TLS server")
	}
	_ = ln.Close()
	if _, err := Dial(testCtx(t), "127.0.0.1", port); err == nil {
		t.Fatal("Dial succeeded against closed port")
	}
}

// TestLiveDevice runs read-only Discover + Status against real devices on the
// LAN. Enable with CAST_LIVE=1.
func TestLiveDevice(t *testing.T) {
	if os.Getenv("CAST_LIVE") == "" {
		t.Skip("set CAST_LIVE=1 to query real devices on the LAN (read-only)")
	}
	devs, err := Discover(context.Background(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) == 0 {
		t.Fatal("no devices discovered")
	}
	for _, d := range devs {
		t.Logf("device: %+v", d)
		c, err := Dial(testCtx(t), d.Host, d.Port)
		if err != nil {
			t.Fatal(err)
		}
		st, err := c.Status(testCtx(t))
		_ = c.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range st.Apps {
			t.Logf("  app: %+v", a)
		}
	}
}

func TestTooLargeMessageKeepsConnection(t *testing.T) {
	f := newFakeDevice(t)
	c := testDial(t, f, defaultConfig)
	ctx := testCtx(t)
	err := c.write(ctx, receiverID, nsReceiver, strings.Repeat("x", maxFrameSize+1))
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("write error = %v", err)
	}
	if c.Err() != nil {
		t.Fatalf("connection killed: %v", c.Err())
	}
	if _, err := c.Status(ctx); err != nil {
		t.Fatalf("status after refused write: %v", err)
	}
}

func TestLaunchErrorIsResponseError(t *testing.T) {
	f := newFakeDevice(t)
	c := testDial(t, f, defaultConfig)
	_, err := c.Launch(testCtx(t), "NOTFOUND")
	var re *ResponseError
	if !errors.As(err, &re) || re.Type != "LAUNCH_ERROR" {
		t.Fatalf("Launch error %v (%T)", err, err)
	}
}
