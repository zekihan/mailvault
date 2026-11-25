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

const (
	mboxDirPerms  = 0o700
	mboxFilePerms = 0o600
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

func NewMboxTarget(configMap map[string]any) (plugins.Target, error) {
	limits, err := parseBackupListingLimits(configMap)
	if err != nil {
		return nil, err
	}
	maxMessageBytes, limitErr := config.ParseMessageLimit(configMap)
	if limitErr != nil {
		return nil, limitErr
	}
	path, err := config.ParseString(configMap, config.TargetConfigPath, "")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", config.TargetConfigPath, err)
	}
	create, err := config.ParseBool(configMap, config.TargetConfigCreateIfMissing, true)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", config.TargetConfigCreateIfMissing, err)
	}

	if path == "" {
		return nil, errors.New("path is required")
	}

	// Expand home directory.
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("expand home directory: %w", err)
		}
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
func (target *MboxTarget) SetName(name string) {
	target.name = name
}

func (target *MboxTarget) Name() string {
	return target.name
}

func (target *MboxTarget) Type() string {
	return "mbox"
}

func (target *MboxTarget) DestinationIdentity() string {
	return destinationFingerprint(target.Type(), canonicalDestinationPath(target.path))
}

func (target *MboxTarget) Initialize(ctx context.Context) error {
	target.mu.Lock()
	defer target.mu.Unlock()

	// Do not create either the parent directory or mailbox when disabled.
	dir := filepath.Dir(target.path)
	flags := os.O_RDWR | os.O_APPEND
	if target.create {
		if err := os.MkdirAll(dir, mboxDirPerms); err != nil {
			return fmt.Errorf("create mbox directory: %w", err)
		}
		flags |= os.O_CREATE
	}

	file, err := openMboxFile(target.path, flags, mboxFilePerms)
	if err != nil {
		return fmt.Errorf("open mbox file: %w", err)
	}
	target.file = file

	return nil
}

func (target *MboxTarget) WriteMessage(ctx context.Context, msg *plugins.Message) error {
	_, err := target.WriteBackupMessage(ctx, msg)
	return err
}

func (target *MboxTarget) LastBackupLocator() string {
	target.mu.Lock()
	defer target.mu.Unlock()
	return target.lastLocator
}

func (target *MboxTarget) WriteRepairMessage(ctx context.Context, msg *plugins.Message) (string, error) {
	return target.WriteBackupMessage(ctx, msg)
}

func (target *MboxTarget) WriteBackupMessage(ctx context.Context, msg *plugins.Message) (_ string, err error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	target.lastLocator = ""
	decodedBytes := int64(len(msg.Raw))
	if len(msg.Raw) > 0 && msg.Raw[len(msg.Raw)-1] != '\n' {
		decodedBytes++
	}
	if err := plugins.CheckMessageSize(decodedBytes, target.maxMessageBytes); err != nil {
		return "", err
	}

	if target.file == nil {
		return "", errors.New("mbox not initialized")
	}
	if target.broken != nil {
		return "", target.broken
	}
	if err := lockMbox(ctx, target.file); err != nil {
		return "", fmt.Errorf("lock mbox: %w", err)
	}
	defer func() {
		if unlockErr := unlockMbox(target.file); unlockErr != nil {
			target.broken = fmt.Errorf("unlock mbox: %w", unlockErr)
			err = errors.Join(err, target.broken)
		}
	}()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	info, err := target.statMboxFile()
	if err != nil {
		return "", err
	}
	padding, err := target.mboxBoundaryPadding(info)
	if err != nil {
		return "", err
	}
	raw := escapeMboxrd(msg.Raw)
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		raw = append(raw, '\n')
	}
	record := buildMboxRecord(raw)
	writeData := joinMboxWrite(padding, record)
	if err := target.appendMboxRecord(writeData, info.Size()); err != nil {
		return "", err
	}
	target.lastLocator = fmt.Sprintf("%d:%d:%d:%d", info.Size()+int64(len(padding)), len(record), len(raw), len(msg.Raw))
	return target.lastLocator, nil
}

func buildMboxRecord(raw []byte) []byte {
	fromLine := fmt.Sprintf("From MAILER-DAEMON %s\n", time.Now().Format(time.ANSIC))
	record := append([]byte(fromLine), raw...)
	record = append(record, '\n')
	return record
}

func joinMboxWrite(padding, record []byte) []byte {
	writeData := make([]byte, 0, len(padding)+len(record))
	writeData = append(writeData, padding...)
	writeData = append(writeData, record...)
	return writeData
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

func (target *MboxTarget) Close() error {
	target.mu.Lock()
	defer target.mu.Unlock()

	if target.file != nil {
		err := target.file.Close()
		target.file = nil
		return err
	}
	return nil
}

func (target *MboxTarget) statMboxFile() (os.FileInfo, error) {
	info, err := target.file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat mbox: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("mbox must be a regular file")
	}
	return info, nil
}

func (target *MboxTarget) mboxBoundaryPadding(info os.FileInfo) ([]byte, error) {
	padding := make([]byte, 0, 1)
	if info.Size() == 0 {
		return padding, nil
	}
	var lastByte [1]byte
	if _, err := target.file.ReadAt(lastByte[:], info.Size()-1); err != nil {
		return nil, fmt.Errorf("read mbox boundary: %w", err)
	}
	if lastByte[0] != '\n' {
		padding = []byte{'\n'}
	}
	return padding, nil
}

func (target *MboxTarget) appendMboxRecord(writeData []byte, fileSize int64) error {
	n, writeErr := target.file.Write(writeData)
	if writeErr == nil && n != len(writeData) {
		writeErr = errors.New("short write")
	}
	if writeErr == nil {
		writeErr = target.file.Sync()
	}
	if writeErr != nil {
		target.rollbackMboxAppend(fileSize)
		return errors.Join(fmt.Errorf("append mbox: %w", writeErr), target.broken)
	}
	return nil
}

func (target *MboxTarget) rollbackMboxAppend(fileSize int64) {
	rollbackErr := errors.Join(target.file.Truncate(fileSize), target.file.Sync())
	if rollbackErr != nil {
		target.broken = fmt.Errorf("mbox rollback failed; refusing further appends: %w", rollbackErr)
	}
}
