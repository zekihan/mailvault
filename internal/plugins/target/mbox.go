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
	listingLimits   backupListingLimits
	maxMessageBytes int64
	name            string
	path            string
	create          bool
	file            *os.File
	broken          error
	lastLocator     string
	mu              sync.Mutex
}

func NewMboxTarget(configMap map[string]interface{}) (plugins.Target, error) {
	limits, err := parseBackupListingLimits(configMap)
	if err != nil {
		return nil, err
	}
	maxMessageBytes, limitErr := config.ParseMessageLimit(configMap)
	if limitErr != nil {
		return nil, limitErr
	}
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
		listingLimits:   limits,
		maxMessageBytes: maxMessageBytes,
		name:            "mbox:" + path, // Will be overridden by registry
		path:            path,
		create:          create,
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

func (t *MboxTarget) DestinationIdentity() string {
	return destinationFingerprint(t.Type(), canonicalDestinationPath(t.path))
}

func (t *MboxTarget) Initialize(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Do not create either the parent directory or mailbox when disabled.
	dir := filepath.Dir(t.path)
	flags := os.O_RDWR | os.O_APPEND
	if t.create {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create mbox directory: %w", err)
		}
		flags |= os.O_CREATE
	}

	// Open mbox file for appending
	file, err := openMboxFile(t.path, flags, 0600)
	if err != nil {
		return fmt.Errorf("open mbox file: %w", err)
	}
	t.file = file

	return nil
}

func (t *MboxTarget) WriteMessage(ctx context.Context, msg *plugins.Message) error {
	_, err := t.WriteBackupMessage(ctx, msg)
	return err
}

func (t *MboxTarget) LastBackupLocator() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastLocator
}

func (t *MboxTarget) WriteRepairMessage(ctx context.Context, msg *plugins.Message) (string, error) {
	return t.WriteBackupMessage(ctx, msg)
}

func (t *MboxTarget) WriteBackupMessage(ctx context.Context, msg *plugins.Message) (locator string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastLocator = ""
	// Mbox adds a final newline to unterminated messages. Reserve that byte
	// within the decoded limit so physical listing/verification can read it.
	decodedBytes := int64(len(msg.Raw))
	if len(msg.Raw) > 0 && msg.Raw[len(msg.Raw)-1] != '\n' {
		decodedBytes++
	}
	if err := plugins.CheckMessageSize(decodedBytes, t.maxMessageBytes); err != nil {
		return "", err
	}

	if t.file == nil {
		return "", errors.New("mbox not initialized")
	}
	if t.broken != nil {
		return "", t.broken
	}
	if err := lockMbox(ctx, t.file); err != nil {
		return "", fmt.Errorf("lock mbox: %w", err)
	}
	defer func() {
		if unlockErr := unlockMbox(t.file); unlockErr != nil {
			t.broken = fmt.Errorf("unlock mbox: %w", unlockErr)
			err = errors.Join(err, t.broken)
		}
	}()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	info, err := t.file.Stat()
	if err != nil {
		return "", fmt.Errorf("stat mbox: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("mbox must be a regular file")
	}
	var padding []byte
	if info.Size() > 0 {
		var lastByte [1]byte
		if _, err := t.file.ReadAt(lastByte[:], info.Size()-1); err != nil {
			return "", fmt.Errorf("read mbox boundary: %w", err)
		}
		if lastByte[0] != '\n' {
			padding = []byte{'\n'}
		}
	}

	// Escape every raw line using mboxrd, including malformed headers: the
	// reader recognizes "From " separators throughout the record.
	raw := escapeMboxrd(msg.Raw)
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		raw = append(raw, '\n')
	}

	// Append a complete record under the inode lock. On any failure restore
	// the original length before another writer or retry can append.
	fromLine := fmt.Sprintf("From MAILER-DAEMON %s\n", time.Now().Format(time.ANSIC))
	record := append([]byte(fromLine), raw...)
	record = append(record, '\n')
	writeData := append(padding, record...)
	n, writeErr := t.file.Write(writeData)
	if writeErr == nil && n != len(writeData) {
		writeErr = errors.New("short write")
	}
	if writeErr == nil {
		writeErr = t.file.Sync()
	}
	if writeErr != nil {
		rollbackErr := errors.Join(t.file.Truncate(info.Size()), t.file.Sync())
		if rollbackErr != nil {
			t.broken = fmt.Errorf("mbox rollback failed; refusing further appends: %w", rollbackErr)
		}
		return "", errors.Join(fmt.Errorf("append mbox: %w", writeErr), t.broken)
	}

	t.lastLocator = fmt.Sprintf("%d:%d:%d:%d", info.Size()+int64(len(padding)), len(record), len(raw), len(msg.Raw))
	return t.lastLocator, nil
}

func escapeMboxrd(raw []byte) []byte {
	var out []byte
	for pos := 0; pos < len(raw); {
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
