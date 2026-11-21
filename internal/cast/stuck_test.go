package cast

import (
	"context"
	"net"
	"runtime"
	"testing"
	"time"
)

// stuckClient returns a client on a pipe whose far end never reads, so every
// write blocks until its (one hour) deadline.
func stuckClient(t *testing.T) (*Client, net.Conn) {
	t.Helper()
	near, far := net.Pipe()
	cfg := defaultConfig
	cfg.pingInterval, cfg.deadAfter, cfg.writeTimeout = time.Hour, 2*time.Hour, time.Hour
	c := newClient(near, cfg)
	t.Cleanup(func() { _ = c.Close(); _ = far.Close() })
	return c, far
}

func TestCloseDoesNotWaitForAStuckWrite(t *testing.T) {
	c, _ := stuckClient(t)
	wrote := make(chan error, 1)
	go func() { wrote <- c.Send(context.Background(), receiverID, nsReceiver, `{"type":"GET_STATUS"}`) }()
	waitFor(t, "write to block", func() bool {
		if c.writeMu.TryLock() {
			c.writeMu.Unlock()
			return false
		}
		return true
	})
	closed := make(chan struct{})
	go func() { _ = c.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close waited for the stuck write")
	}
	select {
	case err := <-wrote:
		if err == nil {
			t.Fatal("stuck write reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stuck write not released by Close")
	}
}

func TestPingFloodWithStuckWritesUsesOnePonger(t *testing.T) {
	c, far := stuckClient(t)
	before := runtime.NumGoroutine()
	ping := &castMessage{SourceID: receiverID, DestinationID: senderID, Namespace: nsHeartbeat, PayloadType: payloadString, PayloadUTF8: `{"type":"PING"}`}
	for range 200 {
		if err := writeFrame(far, ping); err != nil {
			t.Fatal(err)
		}
	}
	// The reader handled every PING (writeFrame returns once it is read).
	if n := runtime.NumGoroutine() - before; n > 2 {
		t.Fatalf("%d goroutines added for 200 unanswered PINGs", n)
	}
	select {
	case <-c.Done():
		t.Fatalf("connection died: %v", c.Err())
	default:
	}
}
