package source

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/knadh/go-pop3"
)

// pop3LineConn presents at most one protocol line per Read. This keeps the
// dependency's private bufio.Reader from reading ahead into UIDL rows after
// the preceding command consumes its response. The rows can then be read directly and bounded
// incrementally, without Cmd(..., true) buffering the entire listing.
// Access is serialized by POP3Source.mu.
type pop3LineConn struct {
	net.Conn
	reader     *bufio.Reader
	pending    []byte
	pendingErr error
}

func newPOP3LineConn(conn net.Conn) *pop3LineConn {
	return &pop3LineConn{Conn: conn, reader: bufio.NewReader(conn)}
}

func (c *pop3LineConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(c.pending) == 0 {
		line, err := c.reader.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			err = nil
		}
		c.pending, c.pendingErr = line, err
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	if len(c.pending) == 0 {
		return n, c.pendingErr
	}
	return n, nil
}

// readLine never retains more than the remaining byte budget, even when the
// server sends one unterminated line. The fixed reader adds at most 4 KiB.
func (c *pop3LineConn) readLine(remaining *int) ([]byte, error) {
	var line []byte
	for {
		part, err := c.reader.ReadSlice('\n')
		if len(part) > *remaining {
			return nil, fmt.Errorf("%w: UIDL response byte budget", errListingLimit)
		}
		*remaining -= len(part)
		if len(line)+len(part) > 512 {
			return nil, errors.New("invalid UIDL response line exceeds 512 bytes")
		}
		line = append(line, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return nil, err
		}
		return bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r")), nil
	}
}

func readPOP3UIDLs(conn *pop3.Conn, stream *pop3LineConn, count int, limits listingLimits) ([]pop3.MessageID, error) {
	if err := conn.Send("UIDL"); err != nil {
		return nil, err
	}
	remaining := limits.bytes
	status, err := stream.readLine(&remaining)
	if err != nil {
		return nil, err
	}
	switch {
	case bytes.Equal(status, []byte("+OK")), bytes.HasPrefix(status, []byte("+OK ")):
	case bytes.HasPrefix(status, []byte("-ERR ")):
		return nil, errors.New(string(status[len("-ERR "):]))
	default:
		return nil, errors.New("unknown response: invalid UIDL status")
	}
	seen := make(map[string]bool)
	seenIDs := make(map[int]bool)
	var identities []pop3.MessageID
	for {
		line, err := stream.readLine(&remaining)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(line, []byte(".")) {
			return identities, nil
		}
		if len(identities) >= limits.items || len(identities) >= count {
			return nil, fmt.Errorf("%w: UIDL item budget", errListingLimit)
		}
		fields := bytes.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("malformed UIDL row with %d fields", len(fields))
		}
		id, err := strconv.Atoi(string(fields[0]))
		if err != nil {
			return nil, fmt.Errorf("parse UIDL message number: %w", err)
		}
		uid := string(fields[1])
		if id <= 0 || id > count || seenIDs[id] || seen[uid] {
			return nil, fmt.Errorf("invalid or duplicate UIDL result for message %d", id)
		}
		// RFC 1939 unique-id: 1 to 70 visible ASCII characters.
		if len(uid) == 0 || len(uid) > 70 {
			return nil, fmt.Errorf("invalid UIDL unique-id length")
		}
		for _, ch := range []byte(uid) {
			if ch < 0x21 || ch > 0x7e {
				return nil, fmt.Errorf("invalid UIDL unique-id character")
			}
		}
		seenIDs[id], seen[uid] = true, true
		identities = append(identities, pop3.MessageID{ID: id, UID: uid})
	}
}
