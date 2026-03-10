package tunnel

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/jpillora/chisel/share/cio"
	"github.com/jpillora/chisel/share/settings"
	"golang.org/x/crypto/ssh"
)

// mockSSHTunnel is a controllable sshTunnel for testing.
// Each call to getSSH blocks until a conn is delivered on connCh or ctx is done.
type mockSSHTunnel struct {
	connCh chan ssh.Conn
}

func (m *mockSSHTunnel) getSSH(ctx context.Context) ssh.Conn {
	select {
	case <-ctx.Done():
		return nil
	case c := <-m.connCh:
		return c
	}
}

// newTestUDPListener creates a udpListener bound to a random local UDP port.
func newTestUDPListener(t *testing.T, tun sshTunnel) (*udpListener, *net.UDPAddr) {
	t.Helper()
	a, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenUDP("udp", a)
	if err != nil {
		t.Fatal(err)
	}
	u := &udpListener{
		Logger: cio.NewLogger("test").Fork("udp"),
		sshTun: tun,
		remote: &settings.Remote{
			RemoteHost:  "127.0.0.1",
			RemotePort:  "9999",
			RemoteProto: "udp",
		},
		inbound: conn,
		maxMTU:  1024,
	}
	return u, conn.LocalAddr().(*net.UDPAddr)
}

// sendUDP sends a single byte to addr. Used to unblock runInbound when it is
// waiting in ReadFromUDP after the egCtx has already been cancelled, so the
// errgroup can return quickly without waiting for the 1-second read deadline.
func sendUDP(addr *net.UDPAddr) {
	c, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return
	}
	defer c.Close()
	c.Write([]byte("x"))
}

// TestUDPListenerRunRecovery verifies that udpListener.run() does not propagate
// an SSH channel error (e.g. connection reset by peer) as a fatal error.
// Before the fix, such an error would bubble up through BindRemotes and cancel
// the client's errgroup context, killing the reconnect loop. After the fix,
// run() clears the cached SSH channel and retries transparently.
func TestUDPListenerRunRecovery(t *testing.T) {
	tun := &mockSSHTunnel{connCh: make(chan ssh.Conn, 1)}
	// Pre-load a conn whose OpenChannel always fails, simulating RST.
	tun.connCh <- &mockDeadSSHConn{closed: make(chan struct{})}

	u, localAddr := newTestUDPListener(t, tun)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- u.run(ctx) }()

	// runOutbound receives the failing conn and returns an error quickly.
	// runInbound is blocked in ReadFromUDP with a 1-second deadline; nudge it
	// so the inner errgroup exits without waiting the full second.
	time.Sleep(10 * time.Millisecond)
	sendUDP(localAddr)

	// Allow run() time to process the inner error and enter the next iteration.
	time.Sleep(50 * time.Millisecond)

	// run() must still be alive — it must NOT have returned the SSH error.
	select {
	case err := <-done:
		t.Fatalf("run() exited early after SSH channel error (err=%v); should recover and keep running", err)
	default:
		// pass: run() is still running
	}

	// Cancel the context for a clean shutdown and verify nil return.
	cancel()
	// Nudge runInbound one more time so it unblocks quickly on shutdown.
	sendUDP(localAddr)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() returned non-nil error on clean shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not return after context cancel")
	}
}

// TestUDPListenerRunContextCancel verifies that run() exits cleanly with a nil
// error when the context is cancelled before any SSH connection is available.
func TestUDPListenerRunContextCancel(t *testing.T) {
	// connCh is never written to: getSSH will block until ctx is done.
	tun := &mockSSHTunnel{connCh: make(chan ssh.Conn)}
	u, localAddr := newTestUDPListener(t, tun)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- u.run(ctx) }()

	cancel()
	// Nudge runInbound so it doesn't stall on the read deadline.
	sendUDP(localAddr)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() returned non-nil error on context cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not return after context cancel")
	}
}
