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

func (c *deadlineConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active && c.timeout > 0 {
		limit := time.Now().Add(c.timeout)
		if deadline.IsZero() || limit.Before(deadline) {
			deadline = limit
		}
	}
	return c.Conn.SetReadDeadline(deadline)
}

func (c *deadlineConn) Read(p []byte) (int, error) {
	if err := c.SetReadDeadline(time.Time{}); err != nil {
		c.recordError(err)
		return 0, err
	}
	n, err := c.Conn.Read(p)
	if err != nil {
		c.recordError(err)
	}
	return n, err
}

func (c *deadlineConn) Write(p []byte) (int, error) {
	if c.timeout > 0 {
		if err := c.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
			c.recordError(err)
			return 0, err
		}
	}
	n, err := c.Conn.Write(p)
	if err != nil {
		c.recordError(err)
	}
	return n, err
}

func (c *deadlineConn) recordError(err error) { c.mu.Lock(); c.lastErr = err; c.mu.Unlock() }

func (c *deadlineConn) transportError() error { c.mu.Lock(); defer c.mu.Unlock(); return c.lastErr }

// beginIO must be paired with its returned cleanup before the source lock is
// released. Closing the transport on cancellation interrupts TLS handshakes as
// well as blocked protocol reads/writes, even when the library resets deadlines.
func (c *deadlineConn) beginIO(ctx context.Context) func() {
	c.mu.Lock()
	c.active = true
	c.lastErr = nil
	if c.timeout > 0 {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	}
	c.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	return func() {
		stop()
		c.mu.Lock()
		c.active = false
		_ = c.Conn.SetReadDeadline(time.Time{})
		c.mu.Unlock()
	}
}

type dialFunc func(network, address string) (net.Conn, error)

func (dial dialFunc) Dial(network, address string) (net.Conn, error) {
	return dial(network, address)
}

// switchConn keeps a protocol client's buffered reader and writer attached
// while upgrading its underlying transport to TLS after STARTTLS/STLS.
type switchConn struct {
	net.Conn
	mu      sync.RWMutex
	current net.Conn
}

func (c *switchConn) set(conn net.Conn) {
	c.mu.Lock()
	c.current = conn
	c.mu.Unlock()
}

func (c *switchConn) active() net.Conn {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.current != nil {
		return c.current
	}
	return c.Conn
}

func (c *switchConn) Read(p []byte) (int, error)         { return c.active().Read(p) }
func (c *switchConn) Write(p []byte) (int, error)        { return c.active().Write(p) }
func (c *switchConn) SetDeadline(t time.Time) error      { return c.active().SetDeadline(t) }
func (c *switchConn) SetReadDeadline(t time.Time) error  { return c.active().SetReadDeadline(t) }
func (c *switchConn) SetWriteDeadline(t time.Time) error { return c.active().SetWriteDeadline(t) }
func (c *switchConn) Close() error                       { return c.Conn.Close() }
