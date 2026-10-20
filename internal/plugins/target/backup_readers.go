package target

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/zekihan/mailvault/internal/plugins"
)

const maxBackupMessageReadBytes = 512 << 20

func (t *MaildirTarget) ListBackupMessages(ctx context.Context) ([]plugins.BackupMessage, error) {
	var result []plugins.BackupMessage
	if _, err := os.Stat(t.path); os.IsNotExist(err) {
		return result, nil
	} else if err != nil {
		return nil, err
	}
	err := filepath.WalkDir(t.path, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "tmp" {
				return filepath.SkipDir
			}
			return nil
		}
		parent := filepath.Base(filepath.Dir(path))
		if parent != "cur" && parent != "new" {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(t.path, path)
		if err != nil {
			return err
		}
		result = append(result, plugins.BackupMessage{Target: t.Name(), Locator: filepath.ToSlash(rel)})
		return nil
	})
	sort.Slice(result, func(i, j int) bool { return result[i].Locator < result[j].Locator })
	return result, err
}

func (t *MaildirTarget) ReadBackupMessage(ctx context.Context, locator string) (*plugins.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	clean := filepath.Clean(filepath.FromSlash(locator))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("invalid maildir locator")
	}
	path, err := t.ResolveBackupLocator(locator)
	if err != nil {
		return nil, err
	}
	rootResolved, err := filepath.EvalSymlinks(t.path)
	if err != nil {
		return nil, err
	}
	pathResolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	if pathResolved != rootResolved && !strings.HasPrefix(pathResolved, rootResolved+string(filepath.Separator)) {
		return nil, fmt.Errorf("maildir locator escapes target root")
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %v", plugins.ErrBackupMessageMissing, err)
		}
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("maildir locator is not a regular file")
	}
	if info.Size() > maxBackupMessageReadBytes {
		return nil, fmt.Errorf("maildir message exceeds the %d byte verification limit", maxBackupMessageReadBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	msg := backupMessageFromRaw(raw)
	msg.Folder = maildirFolder(t.path, path)
	return msg, nil
}

func maildirFolder(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 3 || parts[0] != ".folders" {
		return ""
	}
	var folders []string
	for _, part := range parts[1:] {
		if part == "cur" || part == "new" {
			break
		}
		decoded, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil {
			return ""
		}
		folders = append(folders, string(decoded))
	}
	return strings.Join(folders, "/")
}

// ResolveBackupLocator follows Maildir's allowed new/cur move and flag suffix rename.
func (t *MaildirTarget) ResolveBackupLocator(locator string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(locator))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid maildir locator")
	}
	path := filepath.Join(t.path, clean)
	if _, err := os.Lstat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(filepath.Dir(path))
	base := filepath.Base(path)
	if i := strings.Index(base, ":2,"); i >= 0 {
		base = base[:i]
	}
	var matches []string
	for _, subdir := range []string{"cur", "new"} {
		dir := filepath.Join(parent, subdir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", err
		}
		for _, entry := range entries {
			if entry.Type().IsRegular() && (entry.Name() == base || strings.HasPrefix(entry.Name(), base+":2,")) {
				matches = append(matches, filepath.Join(dir, entry.Name()))
			}
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("%w: %s", plugins.ErrBackupMessageMissing, locator)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("maildir locator %q has multiple matching files", locator)
	}
	return matches[0], nil
}

func (t *MboxTarget) ListBackupMessages(ctx context.Context) ([]plugins.BackupMessage, error) {
	return t.listBackupMessages(ctx, maxBackupMessageReadBytes)
}

func (t *MboxTarget) listBackupMessages(ctx context.Context, maxRecordBytes int64) ([]plugins.BackupMessage, error) {
	file, err := os.Open(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	var starts []int64
	reader := bufio.NewReader(file)
	var offset int64
	lineStart := true
	for {
		fragment, readErr := reader.ReadSlice('\n')
		if lineStart && len(fragment) > 0 {
			if bytes.HasPrefix(fragment, []byte("From ")) {
				starts = append(starts, offset)
			}
			lineStart = false
		}
		switch readErr {
		case bufio.ErrBufferFull:
			// Keep scanning oversized lines in bounded fragments.
		case nil, io.EOF:
			lineStart = true
		default:
			return nil, readErr
		}
		offset += int64(len(fragment))
		if len(starts) > 0 && offset-starts[len(starts)-1] > maxRecordBytes {
			return nil, fmt.Errorf("%w: mbox record exceeds the %d byte verification limit", plugins.ErrBackupMessageCorrupt, maxRecordBytes)
		}
		if len(starts) == 0 && offset > maxRecordBytes {
			return nil, fmt.Errorf("%w: mbox has no initial separator within the %d byte verification limit", plugins.ErrBackupMessageCorrupt, maxRecordBytes)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if readErr == io.EOF {
			break
		}
	}
	if info.Size() > 0 && (len(starts) == 0 || starts[0] != 0) {
		return nil, fmt.Errorf("%w: mbox record missing initial separator", plugins.ErrBackupMessageCorrupt)
	}
	result := make([]plugins.BackupMessage, 0, len(starts))
	for i, start := range starts {
		end := info.Size()
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		length := end - start
		if length > maxRecordBytes {
			return nil, fmt.Errorf("%w: mbox record exceeds the %d byte verification limit", plugins.ErrBackupMessageCorrupt, maxRecordBytes)
		}
		record := make([]byte, length)
		n, readErr := file.ReadAt(record, start)
		if readErr != nil && readErr != io.EOF {
			return nil, readErr
		}
		record = record[:n]
		lineEnd := bytes.IndexByte(record, '\n')
		if lineEnd < 0 {
			result = append(result, plugins.BackupMessage{Target: t.Name(), Locator: fmt.Sprintf("%d:%d:0:0", start, length)})
			continue
		}
		encodedLength := len(record) - lineEnd - 2
		if encodedLength < 0 {
			encodedLength = 0
		}
		if encodedLength > 0 && record[lineEnd+1+encodedLength-1] != '\n' {
			encodedLength = 0
		}
		decodedLength := len(unescapeMboxrd(record[lineEnd+1 : lineEnd+1+encodedLength]))
		result = append(result, plugins.BackupMessage{Target: t.Name(), Locator: fmt.Sprintf("%d:%d:%d:%d", start, length, encodedLength, decodedLength)})
	}
	return result, ctx.Err()
}

func (t *MboxTarget) CanonicalBackupLocator(locator string) string {
	parts := strings.Split(locator, ":")
	if len(parts) >= 2 {
		return parts[0] + ":" + parts[1]
	}
	return locator
}

func (t *MboxTarget) ReadBackupMessage(ctx context.Context, locator string) (*plugins.Message, error) {
	parts := strings.Split(locator, ":")
	if len(parts) != 4 {
		return nil, fmt.Errorf("%w: invalid mbox locator", plugins.ErrBackupMessageCorrupt)
	}
	offset, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || offset < 0 {
		return nil, fmt.Errorf("%w: invalid mbox locator", plugins.ErrBackupMessageCorrupt)
	}
	length, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || length <= 0 {
		return nil, fmt.Errorf("%w: invalid mbox locator", plugins.ErrBackupMessageCorrupt)
	}
	encodedLength, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || encodedLength < 0 || encodedLength > length {
		return nil, fmt.Errorf("%w: invalid mbox locator", plugins.ErrBackupMessageCorrupt)
	}
	rawLength, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || rawLength < 0 || rawLength > encodedLength {
		return nil, fmt.Errorf("%w: invalid mbox locator", plugins.ErrBackupMessageCorrupt)
	}
	file, err := os.Open(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %v", plugins.ErrBackupMessageMissing, err)
		}
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if offset > info.Size() || length > info.Size()-offset || length > int64(^uint(0)>>1) {
		return nil, fmt.Errorf("%w: mbox locator exceeds file bounds", plugins.ErrBackupMessageCorrupt)
	}
	if length > maxBackupMessageReadBytes {
		return nil, fmt.Errorf("mbox message exceeds the %d byte verification limit", maxBackupMessageReadBytes)
	}
	data := make([]byte, length)
	n, err := file.ReadAt(data, offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	data = data[:n]
	if len(data) == 0 || !bytes.HasPrefix(data, []byte("From ")) {
		return nil, fmt.Errorf("%w: mbox record missing separator", plugins.ErrBackupMessageCorrupt)
	}
	lineEnd := bytes.IndexByte(data, '\n')
	if lineEnd < 0 {
		return nil, fmt.Errorf("%w: invalid mbox separator", plugins.ErrBackupMessageCorrupt)
	}
	if int64(len(data)-lineEnd-1) < encodedLength+1 {
		return nil, fmt.Errorf("%w: truncated mbox record", plugins.ErrBackupMessageCorrupt)
	}
	encoded := data[lineEnd+1 : lineEnd+1+int(encodedLength)]
	raw := unescapeMboxrd(encoded)
	if int64(len(raw)) < rawLength {
		return nil, fmt.Errorf("%w: invalid mbox raw length", plugins.ErrBackupMessageCorrupt)
	}
	raw = raw[:rawLength]
	return backupMessageFromRaw(raw), ctx.Err()
}

func (t *S3Target) ListBackupMessages(ctx context.Context) ([]plugins.BackupMessage, error) {
	ctx, cancel := t.operationContext(ctx)
	defer cancel()
	t.mu.Lock()
	client, prefix := t.client, t.prefix
	t.mu.Unlock()
	if client == nil {
		return nil, fmt.Errorf("S3 target not initialized")
	}
	var result []plugins.BackupMessage
	var token *string
	for {
		page, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(t.bucket), Prefix: aws.String(prefix), ContinuationToken: token})
		if err != nil {
			return nil, err
		}
		for _, object := range page.Contents {
			if object.Key != nil {
				result = append(result, plugins.BackupMessage{Target: t.Name(), Locator: *object.Key})
			}
		}
		if !aws.ToBool(page.IsTruncated) {
			break
		}
		if page.NextContinuationToken == nil || (token != nil && *token == *page.NextContinuationToken) {
			return nil, fmt.Errorf("S3 listing returned no advancing continuation token")
		}
		token = page.NextContinuationToken
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Locator < result[j].Locator })
	return result, nil
}

func (t *S3Target) ReadBackupMessage(ctx context.Context, locator string) (*plugins.Message, error) {
	ctx, cancel := t.operationContext(ctx)
	defer cancel()
	t.mu.Lock()
	client, prefix := t.client, t.prefix
	t.mu.Unlock()
	if client == nil {
		return nil, fmt.Errorf("S3 target not initialized")
	}
	if !strings.HasPrefix(locator, prefix) {
		return nil, fmt.Errorf("S3 locator is outside configured prefix")
	}
	object, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(t.bucket), Key: aws.String(locator)})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NoSuchKey" || apiErr.ErrorCode() == "NotFound") {
			return nil, fmt.Errorf("%w: %v", plugins.ErrBackupMessageMissing, err)
		}
		return nil, err
	}
	defer func() { _ = object.Body.Close() }()
	if object.ContentLength != nil && *object.ContentLength > maxBackupMessageReadBytes {
		return nil, fmt.Errorf("S3 message exceeds the %d byte verification limit", maxBackupMessageReadBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(object.Body, maxBackupMessageReadBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBackupMessageReadBytes {
		return nil, fmt.Errorf("S3 message exceeds the %d byte verification limit", maxBackupMessageReadBytes)
	}
	msg := backupMessageFromRaw(raw)
	if object.Metadata != nil {
		msg.Folder = object.Metadata["source-folder"]
		msg.UID = object.Metadata["source-uid"]
		msg.MessageID = object.Metadata["message-id"]
		msg.InternalDate = object.Metadata["internal-date"]
	}
	return msg, nil
}

func backupMessageFromRaw(raw []byte) *plugins.Message {
	hash := sha256.Sum256(raw)
	msg := &plugins.Message{Raw: raw, ContentHash: hex.EncodeToString(hash[:]), Size: int64(len(raw))}
	if parsed, err := mail.ReadMessage(bytes.NewReader(raw)); err == nil {
		msg.MessageID = parsed.Header.Get("Message-ID")
	}
	return msg
}

func unescapeMboxrd(raw []byte) []byte {
	var out bytes.Buffer
	for pos := 0; pos < len(raw); {
		end := bytes.IndexByte(raw[pos:], '\n')
		stop := len(raw)
		if end >= 0 {
			stop = pos + end + 1
		}
		line := raw[pos:stop]
		i := 0
		for i < len(line) && line[i] == '>' {
			i++
		}
		if i > 0 && bytes.HasPrefix(line[i:], []byte("From ")) {
			line = line[1:]
		}
		out.Write(line)
		pos = stop
	}
	return out.Bytes()
}
