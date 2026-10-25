package source

import (
	"bufio"
	"bytes"
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
	c := &pop3Protocol{conn: conn, reader: bufio.NewReader(conn), limits: limits}
	_, err := c.ReadOne()
	return c, err
}

func (c *pop3Protocol) Send(command string) error {
	_, err := io.WriteString(c.conn, command+"\r\n")
	return err
}

func (c *pop3Protocol) ReadOne() ([]byte, error) {
	remaining := c.limits.defaults().bytes
	line, err := c.readListingLine(&remaining)
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

func (c *pop3Protocol) Cmd(command string, args ...interface{}) (*bytes.Buffer, error) {
	for _, arg := range args {
		command += " " + fmt.Sprint(arg)
	}
	if err := c.Send(command); err != nil {
		return nil, err
	}
	status, err := c.ReadOne()
	if err != nil {
		return nil, err
	}
	return bytes.NewBuffer(status), nil
}

// readBody counts decoded RFC822 bytes before growing the buffer, including
// CRLFs. ReadSlice bounds scratch space even for an endless line. On overflow
// the caller closes the session rather than draining an untrusted stream.
func (c *pop3Protocol) readBody(limit int64) ([]byte, error) {
	var body bytes.Buffer
	lineStart := true
	previousCR := false
	for {
		fragment, err := c.reader.ReadSlice('\n')
		if err != nil && err != bufio.ErrBufferFull {
			return nil, err
		}
		if lineStart {
			if bytes.Equal(fragment, []byte(".\r\n")) || bytes.Equal(fragment, []byte(".\n")) {
				return body.Bytes(), nil
			}
			if len(fragment) > 0 && fragment[0] == '.' {
				fragment = fragment[1:]
			}
		}
		lineStart = err == nil
		// Preserve CRLF even when its CR and LF straddle reader fragments.
		hasCR := (len(fragment) >= 2 && fragment[len(fragment)-2] == '\r') || (len(fragment) == 1 && previousCR)
		addCRLF := err == nil && !hasCR
		previousCR = len(fragment) > 0 && fragment[len(fragment)-1] == '\r'
		if addCRLF {
			fragment = fragment[:len(fragment)-1]
		}
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

func (c *pop3Protocol) retrieve(id int, limit int64) ([]byte, error) {
	response, err := c.Cmd("LIST", id)
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
	if _, err := c.Cmd("RETR", id); err != nil {
		return nil, err
	}
	return c.readBody(plugins.MessageLimit(limit))
}

func (c *pop3Protocol) Auth(user, password string) error {
	for _, command := range []string{"USER " + user, "PASS " + password, "NOOP"} {
		if _, err := c.Cmd(command); err != nil {
			return err
		}
	}
	return nil
}

func (c *pop3Protocol) Stat() (int, int, error) {
	response, err := c.Cmd("STAT")
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
	size, err := strconv.Atoi(string(fields[1]))
	return count, size, err
}

func (c *pop3Protocol) Quit() error {
	defer func() { _ = c.conn.Close() }()
	_, err := c.Cmd("QUIT")
	return err
}
