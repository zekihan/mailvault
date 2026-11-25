package source

import (
	"context"
	"net"
	"sync"
	"time"
)

// deadlineConn enforces an inactivity timeout only while a source operation is
// active. IMAP has a background reader, so idle time between operations must not
// consume the next command's read budget.
type deadlineConn struct {
	net.Conn

	mu      sync.Mutex
	timeout time.Duration
	active  bool
	lastErr error
}

func (conn *deadlineConn) SetReadDeadline(deadline time.Time) error {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.active && conn.timeout > 0 {
		limit := time.Now().Add(conn.timeout)
		if deadline.IsZero() || limit.Before(deadline) {
			deadline = limit
		}
	}
	return conn.Conn.SetReadDeadline(deadline)
}

func (conn *deadlineConn) Read(p []byte) (int, error) {
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		conn.recordError(err)
		return 0, err
	}
	n, err := conn.Conn.Read(p)
	if err != nil {
		conn.recordError(err)
	}
	return n, err
}

func (conn *deadlineConn) Write(data []byte) (int, error) {
	if conn.timeout > 0 {
		if err := conn.SetWriteDeadline(time.Now().Add(conn.timeout)); err != nil {
			conn.recordError(err)
			return 0, err
		}
	}
	n, err := conn.Conn.Write(data)
	if err != nil {
		conn.recordError(err)
	}
	return n, err
}

func (conn *deadlineConn) recordError(err error) {
	conn.mu.Lock()
	conn.lastErr = err
	conn.mu.Unlock()
}

func (conn *deadlineConn) transportError() error {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	return conn.lastErr
}

// beginIO must be paired with its returned cleanup before the source lock is
// released. Closing the transport on cancellation interrupts TLS handshakes as
// well as blocked protocol reads/writes, even when the library resets deadlines.
func (conn *deadlineConn) beginIO(ctx context.Context) func() {
	conn.mu.Lock()
	conn.active = true
	conn.lastErr = nil
	if conn.timeout > 0 {
		_ = conn.Conn.SetReadDeadline(time.Now().Add(conn.timeout)) //nolint:errcheck // best-effort; a dead conn fails the following read
	}
	conn.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	return func() {
		stop()
		conn.mu.Lock()
		conn.active = false
		_ = conn.Conn.SetReadDeadline(time.Time{}) //nolint:errcheck // best-effort; a dead conn fails the following read
		conn.mu.Unlock()
	}
}

// switchConn keeps a protocol client's buffered reader and writer attached
// while upgrading its underlying transport to TLS after STARTTLS/STLS.
type switchConn struct {
	net.Conn

	mu      sync.RWMutex
	current net.Conn
}

func (conn *switchConn) Read(p []byte) (int, error)         { return conn.active().Read(p) }
func (conn *switchConn) Write(p []byte) (int, error)        { return conn.active().Write(p) }
func (conn *switchConn) SetDeadline(t time.Time) error      { return conn.active().SetDeadline(t) }
func (conn *switchConn) SetReadDeadline(t time.Time) error  { return conn.active().SetReadDeadline(t) }
func (conn *switchConn) SetWriteDeadline(t time.Time) error { return conn.active().SetWriteDeadline(t) }
func (conn *switchConn) Close() error                       { return conn.Conn.Close() }

func (conn *switchConn) set(nextConn net.Conn) {
	conn.mu.Lock()
	conn.current = nextConn
	conn.mu.Unlock()
}

func (conn *switchConn) active() net.Conn {
	conn.mu.RLock()
	defer conn.mu.RUnlock()
	if conn.current != nil {
		return conn.current
	}
	return conn.Conn
}
