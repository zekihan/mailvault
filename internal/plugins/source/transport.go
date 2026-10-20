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
		return 0, err
	}
	return c.Conn.Read(p)
}

func (c *deadlineConn) Write(p []byte) (int, error) {
	if c.timeout > 0 {
		if err := c.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
			return 0, err
		}
	}
	return c.Conn.Write(p)
}

// beginIO must be paired with its returned cleanup before the source lock is
// released. Closing the transport on cancellation interrupts TLS handshakes as
// well as blocked protocol reads/writes, even when the library resets deadlines.
func (c *deadlineConn) beginIO(ctx context.Context) func() {
	c.mu.Lock()
	c.active = true
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
