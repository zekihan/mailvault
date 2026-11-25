package source

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/zekihan/mailvault/internal/plugins"
)

// POP3 commands share one reader so status-line read-ahead remains visible to
// bounded listing and body readers, including bytes prefetched across commands.
type pop3Protocol struct {
	conn   net.Conn
	reader *bufio.Reader
	limits listingLimits
}

func newPOP3Protocol(conn net.Conn, limits listingLimits) (*pop3Protocol, error) {
	proto := &pop3Protocol{conn: conn, reader: bufio.NewReader(conn), limits: limits}
	_, err := proto.ReadOne()
	return proto, err
}

func (proto *pop3Protocol) Send(command string) error {
	_, err := io.WriteString(proto.conn, command+"\r\n")
	return err
}

func (proto *pop3Protocol) ReadOne() ([]byte, error) {
	remaining := proto.limits.defaults().bytes
	line, err := proto.readListingLine(&remaining)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(line, []byte("+OK")) {
		return nil, nil
	}
	if bytes.HasPrefix(line, []byte("+OK ")) {
		return bytes.Clone(line[4:]), nil
	}
	if bytes.HasPrefix(line, []byte("-ERR")) {
		return nil, errors.New(strings.TrimSpace(string(line[4:])))
	}
	return nil, fmt.Errorf("unknown response: %s", line)
}

func (proto *pop3Protocol) Cmd(command string, args ...any) (*bytes.Buffer, error) {
	var sb strings.Builder
	sb.WriteString(command)
	for _, arg := range args {
		sb.WriteString(" ")
		fmt.Fprint(&sb, arg)
	}
	command = sb.String()
	if err := proto.Send(command); err != nil {
		return nil, err
	}
	status, err := proto.ReadOne()
	if err != nil {
		return nil, err
	}
	return bytes.NewBuffer(status), nil
}

func isPOP3BodyTerminator(fragment []byte, lineStart bool) bool {
	return lineStart && (bytes.Equal(fragment, []byte(".\r\n")) || bytes.Equal(fragment, []byte(".\n")))
}

func unescapePOP3BodyFragment(fragment []byte, lineStart bool) ([]byte, bool) {
	if lineStart && len(fragment) > 0 && fragment[0] == '.' {
		return fragment[1:], lineStart
	}
	return fragment, lineStart
}

func normalizePOP3BodyCRLF(fragment []byte, previousCR, atLineEnd bool) ([]byte, bool) {
	hasCR := (len(fragment) >= 2 && fragment[len(fragment)-2] == '\r') || (len(fragment) == 1 && previousCR)
	addCRLF := atLineEnd && !hasCR
	if addCRLF {
		return fragment[:len(fragment)-1], true
	}
	return fragment, false
}

func (proto *pop3Protocol) Auth(user, password string) error {
	for _, command := range []string{"USER " + user, "PASS " + password, "NOOP"} {
		if _, err := proto.Cmd(command); err != nil {
			return err
		}
	}
	return nil
}

func (proto *pop3Protocol) Stat() (int, int64, error) {
	response, err := proto.Cmd("STAT")
	if err != nil {
		return 0, 0, err
	}
	fields := bytes.Fields(response.Bytes())
	if len(fields) != 2 {
		return 0, 0, errors.New("malformed STAT response")
	}
	count, err := strconv.Atoi(string(fields[0]))
	if err != nil || count < 0 {
		return 0, 0, errors.New("invalid STAT count")
	}
	// The mailbox byte total can exceed a 32-bit int even when every
	// individual message is small enough to retrieve.
	size, err := strconv.ParseInt(string(fields[1]), 10, 64)
	return count, size, err
}

func (proto *pop3Protocol) Quit() error {
	defer func() { _ = proto.conn.Close() }()
	_, err := proto.Cmd("QUIT")
	return err
}

// readBody counts decoded RFC822 bytes before growing the buffer, including
// CRLFs. ReadSlice bounds scratch space even for an endless line. On overflow
// the caller closes the session rather than draining an untrusted stream.
func (proto *pop3Protocol) readBody(limit int64) ([]byte, error) {
	var body bytes.Buffer
	lineStart := true
	previousCR := false
	for {
		fragment, err := proto.reader.ReadSlice('\n')
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
		if isPOP3BodyTerminator(fragment, lineStart) {
			return body.Bytes(), nil
		}
		fragment, _ = unescapePOP3BodyFragment(fragment, lineStart)
		lineStart = err == nil
		// Preserve CRLF even when its CR and LF straddle reader fragments.
		fragment, addCRLF := normalizePOP3BodyCRLF(fragment, previousCR, err == nil)
		previousCR = len(fragment) > 0 && fragment[len(fragment)-1] == '\r'
		size := int64(body.Len()) + int64(len(fragment))
		if addCRLF {
			size += 2
		}
		if err := plugins.CheckMessageSize(size, limit); err != nil {
			return nil, err
		}
		_, _ = body.Write(fragment)
		if addCRLF {
			_, _ = body.WriteString("\r\n")
		}
	}
}

func (proto *pop3Protocol) startTLS(ctx context.Context, config *tls.Config) error {
	if _, err := proto.Cmd("STLS"); err != nil {
		return err
	}
	// No POP3 responses may cross the plaintext/TLS boundary. In particular,
	// prefetched +OK lines must not satisfy authentication commands after TLS.
	if proto.reader.Buffered() != 0 {
		return errors.New("unexpected buffered plaintext after STLS response")
	}
	secure := tls.Client(proto.conn, config)
	if err := secure.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	proto.conn = secure
	proto.reader.Reset(secure)
	return nil
}

func (proto *pop3Protocol) retrieve(id int, limit int64) ([]byte, error) {
	response, err := proto.Cmd("LIST", id)
	if err != nil {
		return nil, fmt.Errorf("LIST: %w", err)
	}
	fields := bytes.Fields(response.Bytes())
	if len(fields) != 2 || string(fields[0]) != strconv.Itoa(id) {
		return nil, errors.New("invalid LIST response")
	}
	size, err := strconv.ParseInt(string(fields[1]), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid LIST size: %w", err)
	}
	if err := plugins.CheckMessageSize(size, limit); err != nil {
		return nil, err
	}
	if _, err := proto.Cmd("RETR", id); err != nil {
		return nil, err
	}
	return proto.readBody(plugins.MessageLimit(limit))
}
