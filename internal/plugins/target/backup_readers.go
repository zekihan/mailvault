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
	entries, err := collectBackupMessages(ctx, t.WalkBackupMessages)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Locator < entries[j].Locator })
	return entries, err
}

func (t *MaildirTarget) WalkBackupMessages(ctx context.Context, visit func(plugins.BackupMessage) error) error {
	// Bound traversal as well as message locators: non-message entries can also
	// be controlled by a writer. Neither WalkDir nor ReadDir(path) is bounded.
	traversal := t.listingLimits.budget()
	var walk func(string) error
	walk = func(path string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir, err := openBackupDirectory(path)
		if err != nil {
			return err
		}
		defer func() { _ = dir.Close() }()
		for {
			entries, readErr := dir.ReadDir(backupDirectoryBatchSize)
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					return err
				}
				child := filepath.Join(path, entry.Name())
				rel, err := filepath.Rel(t.path, child)
				if err != nil {
					return err
				}
				locator := filepath.ToSlash(rel)
				if err := traversal.add(locator); err != nil {
					return fmt.Errorf("maildir traversal: %w", err)
				}
				if entry.IsDir() {
					if entry.Name() != "tmp" {
						if err := walk(child); err != nil {
							return err
						}
					}
					continue
				}
				parent := filepath.Base(path)
				if (parent != "cur" && parent != "new") || !entry.Type().IsRegular() {
					continue
				}
				if err := visit(plugins.BackupMessage{Target: t.Name(), Locator: locator}); err != nil {
					return err
				}
			}
			if readErr == io.EOF {
				return ctx.Err()
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(t.path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return walk(t.path)
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
	budget := t.listingLimits.budget()
	for _, subdir := range []string{"cur", "new"} {
		dir, err := openBackupDirectory(filepath.Join(parent, subdir))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", err
		}
		err = func() error {
			defer func() { _ = dir.Close() }()
			for {
				entries, readErr := dir.ReadDir(backupDirectoryBatchSize)
				for _, entry := range entries {
					if err := budget.add(entry.Name()); err != nil {
						return err
					}
					if entry.Type().IsRegular() && (entry.Name() == base || strings.HasPrefix(entry.Name(), base+":2,")) {
						matches = append(matches, filepath.Join(parent, subdir, entry.Name()))
						if len(matches) > 1 {
							return fmt.Errorf("maildir locator %q has multiple matching files", locator)
						}
					}
				}
				if readErr == io.EOF {
					return nil
				}
				if readErr != nil {
					return readErr
				}
			}
		}()
		if err != nil {
			return "", err
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("%w: %s", plugins.ErrBackupMessageMissing, locator)
	}
	return matches[0], nil
}

func (t *MboxTarget) ListBackupMessages(ctx context.Context) ([]plugins.BackupMessage, error) {
	return collectBackupMessages(ctx, t.WalkBackupMessages)
}

func (t *MboxTarget) WalkBackupMessages(ctx context.Context, visit func(plugins.BackupMessage) error) error {
	return t.walkBackupMessages(ctx, maxBackupMessageReadBytes, visit)
}

func (t *MboxTarget) listBackupMessages(ctx context.Context, maxRecordBytes int64) ([]plugins.BackupMessage, error) {
	return collectBackupMessages(ctx, func(ctx context.Context, visit func(plugins.BackupMessage) error) error {
		return t.walkBackupMessages(ctx, maxRecordBytes, visit)
	})
}

func (t *MboxTarget) walkBackupMessages(ctx context.Context, maxRecordBytes int64, visit func(plugins.BackupMessage) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	budget := t.listingLimits.budget()
	emit := func(start, end int64) error {
		length := end - start
		if length > maxRecordBytes {
			return fmt.Errorf("%w: mbox record exceeds the %d byte verification limit", plugins.ErrBackupMessageCorrupt, maxRecordBytes)
		}
		encoded, decoded, _, err := mboxRecordLengths(ctx, file, start, length)
		if err != nil {
			return err
		}
		locator := fmt.Sprintf("%d:%d:%d:%d", start, length, encoded, decoded)
		if err := budget.add(locator); err != nil {
			return err
		}
		return visit(plugins.BackupMessage{Target: t.Name(), Locator: locator})
	}
	reader := bufio.NewReader(io.NewSectionReader(file, 0, info.Size()))
	var offset, start int64
	found, lineStart := false, true
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		fragment, readErr := reader.ReadSlice('\n')
		if lineStart && len(fragment) > 0 {
			if bytes.HasPrefix(fragment, []byte("From ")) {
				if found {
					if err := emit(start, offset); err != nil {
						return err
					}
				} else if offset != 0 {
					return fmt.Errorf("%w: mbox record missing initial separator", plugins.ErrBackupMessageCorrupt)
				}
				if budget.entries >= budget.limits.entries {
					return fmt.Errorf("%w: mbox entry count exceeds max_listing_entries=%d", plugins.ErrBackupListingLimit, budget.limits.entries)
				}
				start, found = offset, true
			}
			lineStart = false
		}
		switch readErr {
		case bufio.ErrBufferFull:
		case nil, io.EOF:
			lineStart = true
		default:
			return readErr
		}
		offset += int64(len(fragment))
		if offset-start > maxRecordBytes {
			return fmt.Errorf("%w: mbox record exceeds the %d byte verification limit", plugins.ErrBackupMessageCorrupt, maxRecordBytes)
		}
		if readErr == io.EOF {
			break
		}
	}
	if offset != info.Size() {
		return io.ErrUnexpectedEOF
	}
	if info.Size() > 0 && !found {
		return fmt.Errorf("%w: mbox record missing initial separator", plugins.ErrBackupMessageCorrupt)
	}
	if found {
		if err := emit(start, offset); err != nil {
			return err
		}
	}
	latest, err := file.Stat()
	if err != nil {
		return err
	}
	if latest.Size() < info.Size() {
		return io.ErrUnexpectedEOF
	}
	return ctx.Err()
}

func mboxRecordLengths(ctx context.Context, file *os.File, start, length int64) (encodedLength, decodedLength int64, hasSeparatorNewline bool, err error) {
	const chunkSize = 32 * 1024
	var buf [chunkSize]byte
	var position int64
	var lineEnd int64
	lineFound := false
	for position < length {
		if err := ctx.Err(); err != nil {
			return 0, 0, false, err
		}
		want := int64(len(buf))
		if remaining := length - position; want > remaining {
			want = remaining
		}
		n, readErr := file.ReadAt(buf[:int(want)], start+position)
		if n > 0 {
			if i := bytes.IndexByte(buf[:n], '\n'); i >= 0 {
				lineEnd = position + int64(i)
				lineFound = true
			} else {
				position += int64(n)
			}
		}
		if readErr != nil && readErr != io.EOF {
			return 0, 0, false, readErr
		}
		if n == 0 {
			if position < length {
				return 0, 0, false, io.ErrUnexpectedEOF
			}
			break
		}
		if lineFound {
			break
		}
	}
	if !lineFound {
		return 0, 0, false, nil
	}
	encodedLength = length - lineEnd - 2
	if encodedLength < 0 {
		encodedLength = 0
	}
	if encodedLength > 0 {
		var secondLast [1]byte
		n, readErr := file.ReadAt(secondLast[:], start+length-2)
		if readErr != nil {
			return 0, 0, false, readErr
		}
		if n != 1 {
			return 0, 0, false, io.ErrUnexpectedEOF
		}
		if secondLast[0] != '\n' {
			encodedLength = 0
		}
	}
	if encodedLength == 0 {
		return encodedLength, 0, true, nil
	}

	decodedLength = encodedLength
	section := io.NewSectionReader(file, start+lineEnd+1, encodedLength)
	pattern := []byte("From ")
	var leadingGTs int64
	var matching bool
	var matchIndex int
	var disabledLine bool
	var removed int64
	var scanned int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, 0, false, err
		}
		n, readErr := section.Read(buf[:])
		scanned += int64(n)
		if n == 0 && readErr == nil {
			return 0, 0, false, io.ErrUnexpectedEOF
		}
		for _, b := range buf[:n] {
			if b == '\n' {
				leadingGTs, matching, matchIndex, disabledLine = 0, false, 0, false
				continue
			}
			if disabledLine {
				continue
			}
			if matching {
				if b != pattern[matchIndex] {
					disabledLine = true
					continue
				}
				matchIndex++
				if matchIndex == len(pattern) {
					removed++
					disabledLine = true
				}
				continue
			}
			if leadingGTs > 0 {
				switch b {
				case '>':
					leadingGTs++
				case 'F':
					matching, matchIndex = true, 1
				default:
					disabledLine = true
				}
				continue
			}
			if b == '>' {
				leadingGTs = 1
			} else {
				disabledLine = true
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return 0, 0, false, readErr
		}
	}
	if scanned != encodedLength {
		return 0, 0, false, io.ErrUnexpectedEOF
	}
	decodedLength -= removed
	return encodedLength, decodedLength, true, nil
}

func (t *MboxTarget) CanonicalBackupLocator(locator string) string {
	parts := strings.Split(locator, ":")
	if len(parts) == 4 {
		if offset, err := strconv.ParseInt(parts[0], 10, 64); err == nil && offset >= 0 {
			// Tail repair can change the retained record length, but not its start.
			return strconv.FormatInt(offset, 10)
		}
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
	entries, err := collectBackupMessages(ctx, t.WalkBackupMessages)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Locator < entries[j].Locator })
	return entries, err
}

func (t *S3Target) WalkBackupMessages(ctx context.Context, visit func(plugins.BackupMessage) error) error {
	t.mu.Lock()
	client, prefix := t.client, t.prefix
	t.mu.Unlock()
	if client == nil {
		return fmt.Errorf("S3 target not initialized")
	}
	budget := t.listingLimits.budget()
	var token *string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Consumer work can take longer than one S3 operation. Bound the
		// request and its decoding, without timing out downstream callbacks.
		pageCtx, cancel := t.operationContext(ctx)
		page, err := client.ListObjectsV2(pageCtx, &s3.ListObjectsV2Input{Bucket: aws.String(t.bucket), Prefix: aws.String(prefix), ContinuationToken: token, MaxKeys: aws.Int32(s3ListingPageSize)}, func(options *s3.Options) {
			options.HTTPClient = listingHTTPClient{next: options.HTTPClient}
			// A rejected response must not retry the same oversized page.
			options.Retryer = listingRetryer{Retryer: options.Retryer}
		})
		cancel()
		if err != nil {
			return err
		}
		if len(page.Contents) > s3ListingPageSize {
			return fmt.Errorf("%w: S3 page exceeds requested entry count", plugins.ErrBackupListingLimit)
		}
		for _, object := range page.Contents {
			if object.Key != nil {
				if len(*object.Key) > maxS3ObjectKeyBytes {
					return fmt.Errorf("%w: S3 object key exceeds 1024 bytes", plugins.ErrBackupListingLimit)
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := budget.add(*object.Key); err != nil {
					return err
				}
				if err := visit(plugins.BackupMessage{Target: t.Name(), Locator: *object.Key}); err != nil {
					return err
				}
			}
		}
		if !aws.ToBool(page.IsTruncated) {
			break
		}
		if page.NextContinuationToken == nil || (token != nil && *token == *page.NextContinuationToken) {
			return fmt.Errorf("S3 listing returned no advancing continuation token")
		}
		token = page.NextContinuationToken
	}
	return ctx.Err()
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
