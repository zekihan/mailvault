package target

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/config"
)

// MboxTarget implements the Target interface for mbox format.
type MboxTarget struct {
	name   string
	path   string
	create bool
	file   *os.File
	broken error
	mu     sync.Mutex
}

func NewMboxTarget(configMap map[string]interface{}) (plugins.Target, error) {
	path, _ := config.ParseString(configMap, config.TargetConfigPath, "")
	create, _ := config.ParseBool(configMap, config.TargetConfigCreateIfMissing, true)

	if path == "" {
		return nil, errors.New("path is required")
	}

	// Expand home directory
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, path[2:])
	}

	return &MboxTarget{
		name:   "mbox:" + path, // Will be overridden by registry
		path:   path,
		create: create,
	}, nil
}

// SetName sets the target name (called by registry).
func (t *MboxTarget) SetName(name string) {
	t.name = name
}

func (t *MboxTarget) Name() string {
	return t.name
}

func (t *MboxTarget) Type() string {
	return "mbox"
}

func (t *MboxTarget) Initialize(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Do not create either the parent directory or mailbox when disabled.
	dir := filepath.Dir(t.path)
	flags := os.O_WRONLY | os.O_APPEND
	if t.create {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return fmt.Errorf("create mbox directory: %w", err)
		}
		flags |= os.O_CREATE
	}

	// Open mbox file for appending
	file, err := os.OpenFile(t.path, flags, 0640)
	if err != nil {
		return fmt.Errorf("open mbox file: %w", err)
	}
	t.file = file

	return nil
}

func (t *MboxTarget) WriteMessage(ctx context.Context, msg *plugins.Message) (err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.file == nil {
		return errors.New("mbox not initialized")
	}
	if t.broken != nil {
		return t.broken
	}
	if err := lockMbox(ctx, t.file); err != nil {
		return fmt.Errorf("lock mbox: %w", err)
	}
	defer func() {
		if unlockErr := unlockMbox(t.file); unlockErr != nil {
			t.broken = fmt.Errorf("unlock mbox: %w", unlockErr)
			err = errors.Join(err, t.broken)
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := t.file.Stat()
	if err != nil {
		return fmt.Errorf("stat mbox: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("mbox must be a regular file")
	}

	// Escape body lines using mboxrd: prefix one '>' to each line whose
	// existing leading '>' run is followed by "From ". Headers remain untouched.
	raw := escapeMboxrd(msg.Raw)
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		raw = append(raw, '\n')
	}

	// Append a complete record under the inode lock. On any failure restore
	// the original length before another writer or retry can append.
	fromLine := fmt.Sprintf("From MAILER-DAEMON %s\n", time.Now().Format(time.ANSIC))
	record := append([]byte(fromLine), raw...)
	record = append(record, '\n')
	_, writeErr := t.file.Write(record)
	if writeErr == nil {
		writeErr = t.file.Sync()
	}
	if writeErr != nil {
		rollbackErr := errors.Join(t.file.Truncate(info.Size()), t.file.Sync())
		if rollbackErr != nil {
			t.broken = fmt.Errorf("mbox rollback failed; refusing further appends: %w", rollbackErr)
		}
		return errors.Join(fmt.Errorf("append mbox: %w", writeErr), t.broken)
	}

	return nil
}

func escapeMboxrd(raw []byte) []byte {
	sep := bytes.Index(raw, []byte("\n\n"))
	crlf := bytes.Index(raw, []byte("\r\n\r\n"))
	if crlf >= 0 && (sep < 0 || crlf < sep) {
		sep = crlf + 4
	} else if sep >= 0 {
		sep += 2
	} else {
		// Headerless input is a body.
		sep = 0
	}
	out := append([]byte(nil), raw[:sep]...)
	for pos := sep; pos < len(raw); {
		end := bytes.IndexByte(raw[pos:], '\n')
		lineEnd := len(raw)
		if end >= 0 {
			lineEnd = pos + end + 1
		}
		line := raw[pos:lineEnd]
		content := line
		if len(content) > 0 && content[len(content)-1] == '\n' {
			content = content[:len(content)-1]
		}
		if len(content) > 0 && content[len(content)-1] == '\r' {
			content = content[:len(content)-1]
		}
		i := 0
		for i < len(content) && content[i] == '>' {
			i++
		}
		if bytes.HasPrefix(content[i:], []byte("From ")) {
			out = append(out, '>')
		}
		out = append(out, line...)
		pos = lineEnd
	}
	return out
}

func (t *MboxTarget) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.file != nil {
		err := t.file.Close()
		t.file = nil
		return err
	}
	return nil
}
