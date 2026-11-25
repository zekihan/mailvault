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
	s3Types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/zekihan/mailvault/internal/plugins"
)

const (
	maildirDirCur = "cur"
	maildirDirNew = "new"
)

const (
	mboxRecordHeadroomBytes = 128
	mboxLocatorFieldCount   = 4
)

// Mbox framing and mboxrd escaping can expand a raw message. Keep this
// bounded independently of the decoded RFC822 limit.
func maxMboxRecordBytes(limit int64) int64 {
	return 2*plugins.MessageLimit(limit) + mboxRecordHeadroomBytes
}

func (target *MaildirTarget) ListBackupMessages(ctx context.Context) ([]plugins.BackupMessage, error) {
	entries, err := collectBackupMessages(ctx, target.WalkBackupMessages)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Locator < entries[j].Locator })
	return entries, err
}

func (target *MaildirTarget) WalkBackupMessages(ctx context.Context, visit func(plugins.BackupMessage) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Clone a pinned root while protected from Initialize/Close. The clone is
	// owned by this walk. Callbacks can re-enter Read/Resolve/Write safely.
	target.mu.Lock()
	root, err := target.backupRoot()
	target.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	budget := target.listingLimits.budget()
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
			if entry.Type().IsRegular() && (parent == maildirDirCur || parent == maildirDirNew) {
				return visit(plugins.BackupMessage{Target: target.Name(), Locator: filepath.ToSlash(relative)})
			}
			return nil
		})
	}
	if err := walk(root, ""); err != nil {
		return err
	}
	return ctx.Err()
}

func (target *MaildirTarget) backupRoot() (*maildirDirectory, error) {
	if target.root != nil {
		if err := target.root.validate(); err != nil {
			return nil, err
		}
		return target.root.walk(".", false, nil)
	}
	return openMaildirPath(target.path, false, nil)
}

func (target *MaildirTarget) ReadBackupMessage(ctx context.Context, locator string) (*plugins.Message, error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	return target.readBackupMessage(ctx, locator)
}

func (target *MaildirTarget) readBackupMessage(ctx context.Context, locator string) (*plugins.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := target.backupRoot()
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	file, relative, err := root.resolveMessageBounded(ctx, locator, target.listingLimits)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	raw, err := readMaildirMessage(file, target.maxMessageBytes)
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
		if part == maildirDirCur || part == maildirDirNew {
			break
		}
		// MessagePath uses ~ for an empty component because an empty path
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
func (target *MaildirTarget) ResolveBackupLocator(locator string) (string, error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	root, err := target.backupRoot()
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	file, relative, err := root.resolveMessageBounded(context.Background(), locator, target.listingLimits)
	if err != nil {
		return "", err
	}
	_ = file.Close()
	return filepath.Join(target.path, relative), nil
}

func (target *MboxTarget) ListBackupMessages(ctx context.Context) ([]plugins.BackupMessage, error) {
	return collectBackupMessages(ctx, target.WalkBackupMessages)
}

func (target *MboxTarget) WalkBackupMessages(ctx context.Context, visit func(plugins.BackupMessage) error) error {
	return target.walkBackupMessages(ctx, maxMboxRecordBytes(target.maxMessageBytes), visit)
}

func (target *MboxTarget) listBackupMessages(ctx context.Context, maxRecordBytes int64) ([]plugins.BackupMessage, error) {
	return collectBackupMessages(ctx, func(ctx context.Context, visit func(plugins.BackupMessage) error) error {
		return target.walkBackupMessages(ctx, maxRecordBytes, visit)
	})
}

func (target *MboxTarget) walkBackupMessages(ctx context.Context, maxRecordBytes int64, visit func(plugins.BackupMessage) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := openMboxFile(target.path, os.O_RDONLY, 0)
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
	budget := target.listingLimits.budget()
	walker := &mboxBackupWalker{target: target, file: file, budget: budget, maxRecordBytes: maxRecordBytes, visit: visit}
	reader := bufio.NewReader(io.NewSectionReader(file, 0, info.Size()))
	var offset, start int64
	found, lineStart := false, true
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		fragment, readErr := reader.ReadSlice('\n')
		var sepErr error
		found, start, sepErr = walker.handleBackupFragment(ctx, fragment, lineStart, offset, start, found)
		if sepErr != nil {
			return sepErr
		}
		lineStart = nextMboxLineStart(readErr, lineStart, len(fragment))
		if readErr != nil && !errors.Is(readErr, bufio.ErrBufferFull) && readErr != io.EOF {
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
	return walker.finishBackupWalk(ctx, offset, start, found, info.Size())
}

type mboxBackupWalker struct {
	target         *MboxTarget
	file           *os.File
	budget         *backupListingBudget
	maxRecordBytes int64
	visit          func(plugins.BackupMessage) error
}

func (walker *mboxBackupWalker) emit(ctx context.Context, start, end int64) error {
	length := end - start
	if length > walker.maxRecordBytes {
		return fmt.Errorf("%w: mbox record exceeds the %d byte verification limit", plugins.ErrBackupMessageCorrupt, walker.maxRecordBytes)
	}
	encoded, decoded, _, err := mboxRecordLengths(ctx, walker.file, start, length)
	if err != nil {
		return err
	}
	if err := plugins.CheckMessageSize(decoded, walker.target.maxMessageBytes); err != nil {
		return err
	}
	locator := fmt.Sprintf("%d:%d:%d:%d", start, length, encoded, decoded)
	if err := walker.budget.add(locator); err != nil {
		return err
	}
	return walker.visit(plugins.BackupMessage{Target: walker.target.Name(), Locator: locator})
}

func (walker *mboxBackupWalker) handleBackupFragment(ctx context.Context, fragment []byte, lineStart bool, offset, start int64, found bool) (bool, int64, error) {
	if !lineStart || len(fragment) == 0 {
		return found, start, nil
	}
	if !bytes.HasPrefix(fragment, []byte("From ")) {
		return found, start, nil
	}
	if found {
		if err := walker.emit(ctx, start, offset); err != nil {
			return false, start, err
		}
	} else if offset != 0 {
		return false, start, fmt.Errorf("%w: mbox record missing initial separator", plugins.ErrBackupMessageCorrupt)
	}
	if walker.budget.entries >= walker.budget.limits.entries {
		return false, start, fmt.Errorf("%w: mbox entry count exceeds max_listing_entries=%d", plugins.ErrBackupListingLimit, walker.budget.limits.entries)
	}
	return true, offset, nil
}

func nextMboxLineStart(readErr error, lineStart bool, fragmentLen int) bool {
	if lineStart && fragmentLen > 0 {
		lineStart = false
	}
	switch {
	case errors.Is(readErr, bufio.ErrBufferFull):
		return lineStart
	case readErr == nil, errors.Is(readErr, io.EOF):
		return true
	default:
		return lineStart
	}
}

func (walker *mboxBackupWalker) finishBackupWalk(ctx context.Context, offset, start int64, found bool, fileSize int64) error {
	if offset != fileSize {
		return io.ErrUnexpectedEOF
	}
	if fileSize > 0 && !found {
		return fmt.Errorf("%w: mbox record missing initial separator", plugins.ErrBackupMessageCorrupt)
	}
	if found {
		if err := walker.emit(ctx, start, offset); err != nil {
			return err
		}
	}
	latest, err := walker.file.Stat()
	if err != nil {
		return err
	}
	if latest.Size() < fileSize {
		return io.ErrUnexpectedEOF
	}
	return ctx.Err()
}

func mboxRecordLengths(ctx context.Context, file *os.File, start, length int64) (int64, int64, bool, error) {
	const chunkSize = 32 * 1024
	var buf [chunkSize]byte
	lineEnd, lineFound, err := findMboxRecordLineEnd(ctx, file, start, length, &buf)
	if err != nil {
		return 0, 0, false, err
	}
	if !lineFound {
		return 0, 0, false, nil
	}
	encodedLength, err := mboxEncodedLength(file, start, length, lineEnd)
	if err != nil {
		return 0, 0, false, err
	}
	if encodedLength == 0 {
		return encodedLength, 0, true, nil
	}

	decodedLength := encodedLength
	section := io.NewSectionReader(file, start+lineEnd+1, encodedLength)
	removed, scanned, err := countMboxrdEscapes(ctx, section, &buf)
	if err != nil {
		return 0, 0, false, err
	}
	if scanned != encodedLength {
		return 0, 0, false, io.ErrUnexpectedEOF
	}
	decodedLength -= removed
	return encodedLength, decodedLength, true, nil
}

func findMboxRecordLineEnd(ctx context.Context, file *os.File, start, length int64, buf *[32 * 1024]byte) (int64, bool, error) {
	var position int64
	for position < length {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		want := int64(len(buf))
		if remaining := length - position; want > remaining {
			want = remaining
		}
		readCount, readErr := file.ReadAt(buf[:int(want)], start+position)
		if readCount > 0 {
			if i := bytes.IndexByte(buf[:readCount], '\n'); i >= 0 {
				return position + int64(i), true, nil
			}
			position += int64(readCount)
		}
		if readErr != nil && readErr != io.EOF {
			return 0, false, readErr
		}
		if readCount == 0 {
			if position < length {
				return 0, false, io.ErrUnexpectedEOF
			}
			break
		}
	}
	return 0, false, nil
}

func mboxEncodedLength(file *os.File, start, length, lineEnd int64) (int64, error) {
	encodedLength := max(length-lineEnd-2, 0)
	if encodedLength == 0 {
		return 0, nil
	}
	var secondLast [1]byte
	readCount, readErr := file.ReadAt(secondLast[:], start+length-2)
	if readErr != nil {
		return 0, readErr
	}
	if readCount != 1 {
		return 0, io.ErrUnexpectedEOF
	}
	if secondLast[0] != '\n' {
		return 0, nil
	}
	return encodedLength, nil
}

type mboxrdEscapeScanner struct {
	leadingGTs   int64
	matching     bool
	matchIndex   int
	disabledLine bool
	removed      int64
}

func countMboxrdEscapes(ctx context.Context, section *io.SectionReader, buf *[32 * 1024]byte) (int64, int64, error) {
	scanner := &mboxrdEscapeScanner{}
	var scanned int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		readCount, readErr := section.Read(buf[:])
		scanned += int64(readCount)
		if readCount == 0 && readErr == nil {
			return 0, 0, io.ErrUnexpectedEOF
		}
		for _, byteValue := range buf[:readCount] {
			scanner.scanByte(byteValue)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return 0, 0, readErr
		}
	}
	return scanner.removed, scanned, nil
}

func (scanner *mboxrdEscapeScanner) scanByte(byteValue byte) {
	const pattern = "From "
	if byteValue == '\n' {
		scanner.leadingGTs, scanner.matching, scanner.matchIndex, scanner.disabledLine = 0, false, 0, false
		return
	}
	if scanner.disabledLine {
		return
	}
	if scanner.matching {
		scanner.scanMatchingByte(byteValue, pattern)
		return
	}
	scanner.scanIdleByte(byteValue)
}

func (scanner *mboxrdEscapeScanner) scanMatchingByte(byteValue byte, pattern string) {
	if byteValue != pattern[scanner.matchIndex] {
		scanner.disabledLine = true
		return
	}
	scanner.matchIndex++
	if scanner.matchIndex == len(pattern) {
		scanner.removed++
		scanner.disabledLine = true
	}
}

func (scanner *mboxrdEscapeScanner) scanIdleByte(byteValue byte) {
	if scanner.leadingGTs > 0 {
		switch byteValue {
		case '>':
			scanner.leadingGTs++
		case 'F':
			scanner.matching, scanner.matchIndex = true, 1
		default:
			scanner.disabledLine = true
		}
		return
	}
	if byteValue == '>' {
		scanner.leadingGTs = 1
	} else {
		scanner.disabledLine = true
	}
}

func (target *MboxTarget) CanonicalBackupLocator(locator string) string {
	parts := strings.Split(locator, ":")
	if len(parts) == mboxLocatorFieldCount {
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
func (target *MboxTarget) BackupLocatorsEquivalent(current, recorded string) bool {
	physical, manifest := strings.Split(current, ":"), strings.Split(recorded, ":")
	if len(physical) != 4 || len(manifest) != 4 {
		return false
	}
	for i := range 3 {
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

func (target *MboxTarget) ReadBackupMessage(ctx context.Context, locator string) (*plugins.Message, error) {
	ref, err := parseMboxLocator(locator)
	if err != nil {
		return nil, err
	}
	file, err := openMboxFile(target.path, os.O_RDONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %w", plugins.ErrBackupMessageMissing, err)
		}
		return nil, err
	}
	defer func() { _ = file.Close() }()
	raw, err := target.readMboxRecord(file, ref)
	if err != nil {
		return nil, err
	}
	return backupMessageFromRaw(raw), ctx.Err()
}

type mboxLocatorRef struct {
	offset, length, encodedLength, rawLength int64
}

func parseMboxLocator(locator string) (mboxLocatorRef, error) {
	invalid := func() error { return fmt.Errorf("%w: invalid mbox locator", plugins.ErrBackupMessageCorrupt) }
	parts := strings.Split(locator, ":")
	if len(parts) != mboxLocatorFieldCount {
		return mboxLocatorRef{}, invalid()
	}
	values := make([]int64, mboxLocatorFieldCount)
	for i, part := range parts {
		value, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return mboxLocatorRef{}, invalid()
		}
		values[i] = value
	}
	ref := mboxLocatorRef{offset: values[0], length: values[1], encodedLength: values[2], rawLength: values[3]}
	if ref.offset < 0 || ref.length <= 0 || ref.encodedLength < 0 || ref.encodedLength > ref.length || ref.rawLength < 0 || ref.rawLength > ref.encodedLength {
		return mboxLocatorRef{}, invalid()
	}
	return ref, nil
}

func (target *MboxTarget) readMboxRecord(file *os.File, ref mboxLocatorRef) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if err := checkMboxRecordBounds(target, ref, info.Size()); err != nil {
		return nil, err
	}
	data, err := readMboxRecordBytes(file, ref)
	if err != nil {
		return nil, err
	}
	encoded, err := splitMboxRecord(data, ref)
	if err != nil {
		return nil, err
	}
	return decodeMboxRecord(encoded, ref.rawLength)
}

func checkMboxRecordBounds(target *MboxTarget, ref mboxLocatorRef, fileSize int64) error {
	if ref.offset > fileSize || ref.length > fileSize-ref.offset || ref.length > int64(^uint(0)>>1) {
		return fmt.Errorf("%w: mbox locator exceeds file bounds", plugins.ErrBackupMessageCorrupt)
	}
	if ref.length > maxMboxRecordBytes(target.maxMessageBytes) {
		return fmt.Errorf("%w: mbox record exceeds encoded byte limit", plugins.ErrMessageTooLarge)
	}
	return plugins.CheckMessageSize(ref.rawLength, target.maxMessageBytes)
}

func readMboxRecordBytes(file *os.File, ref mboxLocatorRef) ([]byte, error) {
	data := make([]byte, ref.length)
	n, err := file.ReadAt(data, ref.offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return data[:n], nil
}

func splitMboxRecord(data []byte, ref mboxLocatorRef) ([]byte, error) {
	if len(data) == 0 || !bytes.HasPrefix(data, []byte("From ")) {
		return nil, fmt.Errorf("%w: mbox record missing separator", plugins.ErrBackupMessageCorrupt)
	}
	lineEnd := bytes.IndexByte(data, '\n')
	if lineEnd < 0 {
		return nil, fmt.Errorf("%w: invalid mbox separator", plugins.ErrBackupMessageCorrupt)
	}
	if int64(len(data)) != ref.length || int64(len(data)-lineEnd-1) != ref.encodedLength+1 {
		return nil, fmt.Errorf("%w: truncated mbox record", plugins.ErrBackupMessageCorrupt)
	}
	encoded := data[lineEnd+1 : lineEnd+1+int(ref.encodedLength)]
	if data[len(data)-1] != '\n' || (len(encoded) > 0 && encoded[len(encoded)-1] != '\n') {
		return nil, fmt.Errorf("%w: mbox record missing framing newline", plugins.ErrBackupMessageCorrupt)
	}
	return encoded, nil
}

func decodeMboxRecord(encoded []byte, rawLength int64) ([]byte, error) {
	raw := unescapeMboxrd(encoded)
	// The writer may add one final LF to a nonempty message. Every other
	// decoded byte belongs to the message and must not be silently discarded.
	padded := rawLength > 0 && int64(len(raw)) == rawLength+1 && raw[len(raw)-1] == '\n'
	if int64(len(raw)) != rawLength && !padded {
		return nil, fmt.Errorf("%w: invalid mbox raw length", plugins.ErrBackupMessageCorrupt)
	}
	return raw[:rawLength], nil
}

func (target *S3Target) ListBackupMessages(ctx context.Context) ([]plugins.BackupMessage, error) {
	entries, err := collectBackupMessages(ctx, target.WalkBackupMessages)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Locator < entries[j].Locator })
	return entries, err
}

func (target *S3Target) WalkBackupMessages(ctx context.Context, visit func(plugins.BackupMessage) error) error {
	target.mu.Lock()
	client, prefix := target.client, target.prefix
	target.mu.Unlock()
	if client == nil {
		return errors.New("S3 target not initialized")
	}
	budget := target.listingLimits.budget()
	remaining := target.operationTimeout
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
		page, next, err := target.fetchS3ListingPage(ctx, client, prefix, token, remaining)
		remaining -= next
		if err != nil {
			return err
		}
		if err := target.visitS3ListingPage(ctx, page, budget, visit); err != nil {
			return err
		}
		nextToken, done, err := handleS3ListingPage(page, token)
		if err != nil {
			return err
		}
		if done {
			break
		}
		token = nextToken
	}
	return ctx.Err()
}

func (target *S3Target) fetchS3ListingPage(ctx context.Context, client *s3.Client, prefix string, token *string, remaining time.Duration) (*s3.ListObjectsV2Output, time.Duration, error) {
	started := time.Now()
	pageCtx, cancel := context.WithTimeout(ctx, remaining)
	page, err := client.ListObjectsV2(pageCtx, &s3.ListObjectsV2Input{Bucket: aws.String(target.bucket), Prefix: aws.String(prefix), ContinuationToken: token, MaxKeys: aws.Int32(s3ListingPageSize)}, func(options *s3.Options) {
		options.HTTPClient = listingHTTPClient{next: options.HTTPClient}
		// A rejected response must not retry the same oversized page.
		options.Retryer = listingRetryer{Retryer: options.Retryer}
	})
	cancel()
	return page, time.Since(started), err
}

func (target *S3Target) visitS3ListingPage(ctx context.Context, page *s3.ListObjectsV2Output, budget *backupListingBudget, visit func(plugins.BackupMessage) error) error {
	if len(page.Contents) > s3ListingPageSize {
		return fmt.Errorf("%w: S3 page exceeds requested entry count", plugins.ErrBackupListingLimit)
	}
	for i := range page.Contents {
		if err := target.visitS3ListingObject(ctx, &page.Contents[i], budget, visit); err != nil {
			return err
		}
	}
	return nil
}

func (target *S3Target) visitS3ListingObject(ctx context.Context, object *s3Types.Object, budget *backupListingBudget, visit func(plugins.BackupMessage) error) error {
	if object.Key == nil {
		return nil
	}
	if len(*object.Key) > maxS3ObjectKeyBytes {
		return fmt.Errorf("%w: S3 object key exceeds 1024 bytes", plugins.ErrBackupListingLimit)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := budget.add(*object.Key); err != nil {
		return err
	}
	return visit(plugins.BackupMessage{Target: target.Name(), Locator: *object.Key})
}

func handleS3ListingPage(page *s3.ListObjectsV2Output, token *string) (*string, bool, error) {
	if !aws.ToBool(page.IsTruncated) {
		return nil, true, nil
	}
	if page.NextContinuationToken == nil || (token != nil && *token == *page.NextContinuationToken) {
		return nil, false, errors.New("S3 listing returned no advancing continuation token")
	}
	return page.NextContinuationToken, false, nil
}

func (target *S3Target) ReadBackupMessage(ctx context.Context, locator string) (*plugins.Message, error) {
	ctx, cancel := target.operationContext(ctx)
	defer cancel()
	target.mu.Lock()
	client, prefix := target.client, target.prefix
	target.mu.Unlock()
	if client == nil {
		return nil, errors.New("S3 target not initialized")
	}
	if !strings.HasPrefix(locator, prefix) {
		return nil, errors.New("S3 locator is outside configured prefix")
	}
	object, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(target.bucket), Key: aws.String(locator)})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NoSuchKey" || apiErr.ErrorCode() == "NotFound") {
			return nil, fmt.Errorf("%w: %w", plugins.ErrBackupMessageMissing, err)
		}
		return nil, err
	}
	defer func() { _ = object.Body.Close() }()
	if object.ContentLength != nil {
		if err := plugins.CheckMessageSize(*object.ContentLength, target.maxMessageBytes); err != nil {
			return nil, err
		}
	}
	raw, err := plugins.ReadMessage(object.Body, target.maxMessageBytes)
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
