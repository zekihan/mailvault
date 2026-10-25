package source

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strconv"
)

// readListingLine never retains more than the remaining byte budget, even when the
// server sends one unterminated line. The fixed reader adds at most 4 KiB.
func (c *pop3Protocol) readListingLine(remaining *int) ([]byte, error) {
	var line []byte
	for {
		part, err := c.reader.ReadSlice('\n')
		if len(part) > *remaining {
			return nil, fmt.Errorf("%w: POP3 response byte budget", errListingLimit)
		}
		*remaining -= len(part)
		if len(line)+len(part) > 512 {
			return nil, errors.New("invalid POP3 response line exceeds 512 bytes")
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

func readPOP3UIDLs(conn *pop3Protocol, count int, limits listingLimits) ([]pop3Identity, error) {
	if err := conn.Send("UIDL"); err != nil {
		return nil, err
	}
	remaining := limits.bytes
	status, err := conn.readListingLine(&remaining)
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
	var identities []pop3Identity
	for {
		line, err := conn.readListingLine(&remaining)
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
		identities = append(identities, pop3Identity{ID: id, UID: uid})
	}
}
