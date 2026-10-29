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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/zekihan/mailvault/internal/plugins"
)

// Mbox framing and mboxrd escaping can expand a raw message. Keep this
// bounded independently of the decoded RFC822 limit.
func maxMboxRecordBytes(limit int64) int64 { return 2*plugins.MessageLimit(limit) + 128 }

func (t *MaildirTarget) ListBackupMessages(ctx context.Context) ([]plugins.BackupMessage, error) {
	entries, err := collectBackupMessages(ctx, t.WalkBackupMessages)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Locator < entries[j].Locator })
	return entries, err
}

func (t *MaildirTarget) WalkBackupMessages(ctx context.Context, visit func(plugins.BackupMessage) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Clone a pinned root while protected from Initialize/Close. The clone is
	// owned by this walk. Callbacks can re-enter Read/Resolve/Write safely.
	t.mu.Lock()
	root, err := t.backupRoot()
	t.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	budget := t.listingLimits.budget()
	var walk func(*maildirDirectory, string) error
	walk = func(directory *maildirDirectory, prefix string) error {
		return directory.walkEntries(ctx, budget, prefix, func(entry os.DirEntry) error {
			relative := filepath.Join(prefix, entry.Name())
			if entry.IsDir() {
				if entry.Name() == "tmp" {
					return nil
				}
				child, err := directory.child(entry.Name(), false, nil)
				if err != nil {
					return err
				}
				defer func() { _ = child.Close() }()
				return walk(child, relative)
			}
			parent := filepath.Base(directory.path)
			if entry.Type().IsRegular() && (parent == "cur" || parent == "new") {
				return visit(plugins.BackupMessage{Target: t.Name(), Locator: filepath.ToSlash(relative)})
			}
			return nil
		})
	}
	if err := walk(root, ""); err != nil {
		return err
	}
	return ctx.Err()
}

func (t *MaildirTarget) backupRoot() (*maildirDirectory, error) {
	if t.root != nil {
		if err := t.root.validate(); err != nil {
			return nil, err
		}
		return t.root.walk(".", false, nil)
	}
	return openMaildirPath(t.path, false, nil)
}

func (t *MaildirTarget) ReadBackupMessage(ctx context.Context, locator string) (*plugins.Message, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.readBackupMessage(ctx, locator)
}

func (t *MaildirTarget) readBackupMessage(ctx context.Context, locator string) (*plugins.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := t.backupRoot()
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	file, relative, err := root.resolveMessageBounded(ctx, locator, t.listingLimits)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	raw, err := readMaildirMessage(file, t.maxMessageBytes)
	if err != nil {
		return nil, err
	}
	if err := root.validateMessage(relative, file); err != nil {
		return nil, err
	}
	if err := root.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	msg := backupMessageFromRaw(raw)
	msg.Folder = maildirFolder(".", relative)
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
		// messagePath uses ~ for an empty component because an empty path
		// segment would be collapsed by filepath.Join.
		if part == "~" {
			folders = append(folders, "")
			continue
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
	t.mu.Lock()
	defer t.mu.Unlock()
	root, err := t.backupRoot()
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	file, relative, err := root.resolveMessageBounded(context.Background(), locator, t.listingLimits)
	if err != nil {
		return "", err
	}
	_ = file.Close()
	return filepath.Join(t.path, relative), nil
}

func (t *MboxTarget) ListBackupMessages(ctx context.Context) ([]plugins.BackupMessage, error) {
	return collectBackupMessages(ctx, t.WalkBackupMessages)
}

func (t *MboxTarget) WalkBackupMessages(ctx context.Context, visit func(plugins.BackupMessage) error) error {
	return t.walkBackupMessages(ctx, maxMboxRecordBytes(t.maxMessageBytes), visit)
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
	file, err := openMboxFile(t.path, os.O_RDONLY, 0)
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
		if err := plugins.CheckMessageSize(decoded, t.maxMessageBytes); err != nil {
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

// BackupLocatorsEquivalent requires the parser-derived start, record length and
// encoded length to match the manifest. The parser cannot distinguish a final
// newline added by the writer from an original one, so the recorded raw length
// may be one byte shorter. Offset-only canonicalization is insufficient here:
// it deliberately allows changed extents for restore and repair history.
func (t *MboxTarget) BackupLocatorsEquivalent(current, recorded string) bool {
	physical, manifest := strings.Split(current, ":"), strings.Split(recorded, ":")
	if len(physical) != 4 || len(manifest) != 4 {
		return false
	}
	for i := 0; i < 3; i++ {
		if physical[i] != manifest[i] {
			return false
		}
	}
	currentRaw, err := strconv.ParseInt(physical[3], 10, 64)
	if err != nil || currentRaw < 0 {
		return false
	}
	recordedRaw, err := strconv.ParseInt(manifest[3], 10, 64)
	if err != nil || recordedRaw < 0 {
		return false
	}
	return physical[3] == manifest[3] || (recordedRaw > 0 && recordedRaw == currentRaw-1)
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
	file, err := openMboxFile(t.path, os.O_RDONLY, 0)
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
	if length > maxMboxRecordBytes(t.maxMessageBytes) {
		return nil, fmt.Errorf("%w: mbox record exceeds encoded byte limit", plugins.ErrMessageTooLarge)
	}
	if err := plugins.CheckMessageSize(rawLength, t.maxMessageBytes); err != nil {
		return nil, err
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
	if int64(len(data)) != length || int64(len(data)-lineEnd-1) != encodedLength+1 {
		return nil, fmt.Errorf("%w: truncated mbox record", plugins.ErrBackupMessageCorrupt)
	}
	encoded := data[lineEnd+1 : lineEnd+1+int(encodedLength)]
	if data[len(data)-1] != '\n' || (len(encoded) > 0 && encoded[len(encoded)-1] != '\n') {
		return nil, fmt.Errorf("%w: mbox record missing framing newline", plugins.ErrBackupMessageCorrupt)
	}
	raw := unescapeMboxrd(encoded)
	// The writer may add one final LF to a nonempty message. Every other
	// decoded byte belongs to the message and must not be silently discarded.
	padded := rawLength > 0 && int64(len(raw)) == rawLength+1 && raw[len(raw)-1] == '\n'
	if int64(len(raw)) != rawLength && !padded {
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
	remaining := t.operationTimeout
	if remaining <= 0 {
		remaining = defaultS3OperationTimeout
	}
	var token *string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Preserve a total listing I/O budget, even for endless empty pages.
		// Consumer processing time does not consume this network budget.
		if remaining <= 0 {
			return context.DeadlineExceeded
		}
		started := time.Now()
		pageCtx, cancel := context.WithTimeout(ctx, remaining)
		page, err := client.ListObjectsV2(pageCtx, &s3.ListObjectsV2Input{Bucket: aws.String(t.bucket), Prefix: aws.String(prefix), ContinuationToken: token, MaxKeys: aws.Int32(s3ListingPageSize)}, func(options *s3.Options) {
			options.HTTPClient = listingHTTPClient{next: options.HTTPClient}
			// A rejected response must not retry the same oversized page.
			options.Retryer = listingRetryer{Retryer: options.Retryer}
		})
		cancel()
		remaining -= time.Since(started)
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
	if object.ContentLength != nil {
		if err := plugins.CheckMessageSize(*object.ContentLength, t.maxMessageBytes); err != nil {
			return nil, err
		}
	}
	raw, err := plugins.ReadMessage(object.Body, t.maxMessageBytes)
	if err != nil {
		return nil, err
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
