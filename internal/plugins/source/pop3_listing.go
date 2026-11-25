package source

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strconv"
)

const maxPOP3ResponseLineBytes = 512

// readListingLine never retains more than the remaining byte budget, even when the
// server sends one unterminated line. The fixed reader adds at most 4 KiB.
func (proto *pop3Protocol) readListingLine(remaining *int) ([]byte, error) {
	var line []byte
	for {
		part, err := proto.reader.ReadSlice('\n')
		if len(part) > *remaining {
			return nil, fmt.Errorf("%w: POP3 response byte budget", errListingLimit)
		}
		*remaining -= len(part)
		if len(line)+len(part) > maxPOP3ResponseLineBytes {
			return nil, errors.New("invalid POP3 response line exceeds 512 bytes")
		}
		line = append(line, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
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
	firstByUID := make(map[string]int)
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
		item, err := parsePOP3UIDLRow(line, count, seenIDs)
		if err != nil {
			return nil, err
		}
		// RFC 1939 allows identical copies to share a UIDL. Mark every
		// occurrence so the caller can deduplicate using the retrieved bytes.
		if first, ok := firstByUID[item.UID]; ok {
			identities[first].sharedUID = true
			item.sharedUID = true
		} else {
			firstByUID[item.UID] = len(identities)
		}
		seenIDs[item.ID] = true
		identities = append(identities, item)
	}
}

func parsePOP3UIDLRow(line []byte, count int, seenIDs map[int]bool) (pop3Identity, error) {
	fields := bytes.Fields(line)
	if len(fields) != 2 {
		return pop3Identity{}, fmt.Errorf("malformed UIDL row with %d fields", len(fields))
	}
	id, err := strconv.Atoi(string(fields[0]))
	if err != nil {
		return pop3Identity{}, fmt.Errorf("parse UIDL message number: %w", err)
	}
	uid := string(fields[1])
	if id <= 0 || id > count || seenIDs[id] {
		return pop3Identity{}, fmt.Errorf("invalid or duplicate UIDL message number %d", id)
	}
	if err := validatePOP3UID(uid); err != nil {
		return pop3Identity{}, err
	}
	return pop3Identity{ID: id, UID: uid}, nil
}

func validatePOP3UID(uid string) error {
	// RFC 1939 unique-id: 1 to 70 visible ASCII characters.
	if uid == "" || len(uid) > 70 {
		return errors.New("invalid UIDL unique-id length")
	}
	for _, ch := range []byte(uid) {
		if ch < 0x21 || ch > 0x7e {
			return errors.New("invalid UIDL unique-id character")
		}
	}
	return nil
}
