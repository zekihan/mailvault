package target

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestBackupListingLimitConfiguration(t *testing.T) {
	for _, factory := range []plugins.TargetFactory{NewMaildirTarget, NewMboxTarget, NewS3Target} {
		for _, key := range []string{"max_listing_entries", "max_listing_locator_bytes"} {
			for _, value := range []any{-1, int64(-1), 1.5, math.NaN(), math.Inf(1), float64(1 << 63), "9223372036854775808", "invalid", true} {
				_, err := factory(map[string]any{"path": "fixture", "bucket": "fixture", "region": "us-east-1", key: value})
				if err == nil || !strings.Contains(err.Error(), key) {
					t.Fatalf("%s=%v accepted: %v", key, value, err)
				}
			}
		}
	}
	defaults, err := parseBackupListingLimits(map[string]any{"max_listing_entries": 0, "max_listing_locator_bytes": "0"})
	if err != nil || defaults.entries != defaultMaxBackupEntries || defaults.locatorBytes != defaultMaxBackupLocatorBytes {
		t.Fatalf("defaults=%+v, %v", defaults, err)
	}
	for _, value := range []any{3, int64(3), float64(3), "3"} {
		limits, err := parseBackupListingLimits(map[string]any{"max_listing_entries": value})
		if err != nil || limits.entries != 3 {
			t.Fatalf("limits=%+v, %v", limits, err)
		}
	}
}

type localListingFixture struct {
	kind   string
	reader plugins.TargetBackupReader
	limits *backupListingLimits
	budget backupListingLimits
}

func (fixture *localListingFixture) open(t *testing.T) {
	t.Helper()
	path := filepath.Join(physicalTempDir(t), "archive")
	if fixture.kind == "maildir" {
		fixture.openMaildir(t, path)
		return
	}
	fixture.openMbox(t, path)
}

func (fixture *localListingFixture) openMaildir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, "new"), 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		if err := os.WriteFile(filepath.Join(path, "new", strconv.Itoa(i)), []byte("Subject: fixture\n\nbody\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	target, err := NewMaildirTarget(map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	fixture.reader = mustAssert[*MaildirTarget](t, target)
	fixture.limits = &mustAssert[*MaildirTarget](t, target).listingLimits
}

func (fixture *localListingFixture) openMbox(t *testing.T, path string) {
	t.Helper()
	data := strings.Repeat("From fixture\nSubject: fixture\n\nbody\n\n", 4)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := NewMboxTarget(map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	fixture.reader = mustAssert[*MboxTarget](t, target)
	fixture.limits = &mustAssert[*MboxTarget](t, target).listingLimits
}

func (fixture *localListingFixture) list(t *testing.T) []plugins.BackupMessage {
	t.Helper()
	*fixture.limits = fixture.budget
	entries, err := fixture.reader.ListBackupMessages(t.Context())
	if err != nil || len(entries) != 4 {
		t.Fatalf("listing=%v, %v", entries, err)
	}
	return entries
}

func (fixture *localListingFixture) budgets(t *testing.T) {
	t.Helper()
	fixture.budget = backupListingLimits{entries: 100, locatorBytes: 4096}
	baseline := fixture.list(t)
	fixture.budget = exactListingBudget(baseline, fixture.kind)
	checkExactListingBudget(t, fixture)
	checkSmallListingBudgets(t, fixture)
}

func exactListingBudget(baseline []plugins.BackupMessage, kind string) backupListingLimits {
	totalBytes := int64(0)
	for i := range baseline {
		totalBytes += int64(len(baseline[i].Locator))
	}
	totalEntries := int64(4)
	if kind == "maildir" {
		totalEntries++
		totalBytes += int64(len("new"))
	}
	return backupListingLimits{entries: totalEntries, locatorBytes: totalBytes}
}

func checkExactListingBudget(t *testing.T, fixture *localListingFixture) {
	t.Helper()
	boundary := fixture.list(t)
	_ = boundary
	fixture.limits.locatorBytes--
	if _, err := fixture.reader.ListBackupMessages(t.Context()); !errors.Is(err, plugins.ErrBackupListingLimit) {
		t.Fatalf("one byte over budget: %v", err)
	}
}

func checkSmallListingBudgets(t *testing.T, fixture *localListingFixture) {
	t.Helper()
	*fixture.limits = backupListingLimits{entries: 3, locatorBytes: 4096}
	count := 0
	err := plugins.WalkBackupMessages(t.Context(), fixture.reader, func(plugins.BackupMessage) error { count++; return nil })
	if !errors.Is(err, plugins.ErrBackupListingLimit) || count > 3 || count == 0 {
		t.Fatalf("entry limit: count=%d, err=%v", count, err)
	}
	checkPartialListingHidden(t, fixture)
	*fixture.limits = backupListingLimits{entries: 100, locatorBytes: 10}
	if _, err := fixture.reader.ListBackupMessages(t.Context()); !errors.Is(err, plugins.ErrBackupListingLimit) {
		t.Fatalf("byte limit error=%v", err)
	}
}

func checkPartialListingHidden(t *testing.T, fixture *localListingFixture) {
	t.Helper()
	entries, err := fixture.reader.ListBackupMessages(t.Context())
	if !errors.Is(err, plugins.ErrBackupListingLimit) || entries != nil {
		t.Fatalf("partial list escaped: %v, %v", entries, err)
	}
}

func checkListingEarlyStop(t *testing.T, fixture *localListingFixture) {
	t.Helper()
	*fixture.limits = backupListingLimits{entries: 100, locatorBytes: 4096}
	stop := errors.New("stop callback")
	count := 0
	err := plugins.WalkBackupMessages(t.Context(), fixture.reader, func(plugins.BackupMessage) error { count++; return stop })
	if !errors.Is(err, stop) || count != 1 {
		t.Fatalf("callback stop: count=%d, err=%v", count, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	count = 0
	err = plugins.WalkBackupMessages(ctx, fixture.reader, func(plugins.BackupMessage) error { count++; cancel(); return nil })
	if !errors.Is(err, context.Canceled) || count != 1 {
		t.Fatalf("canceled walk: count=%d, err=%v", count, err)
	}
}

func TestLocalBackupListingBudgetsAndEarlyStop(t *testing.T) {
	for _, kind := range []string{"maildir", "mbox"} {
		t.Run(kind, func(t *testing.T) {
			fixture := &localListingFixture{kind: kind}
			fixture.open(t)
			fixture.budgets(t)
			checkListingEarlyStop(t, fixture)
		})
	}
}

func newListingTestS3(t *testing.T, handler http.HandlerFunc, limits backupListingLimits) *S3Target {
	t.Helper()
	isolateS3AWS(t)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	target, err := NewS3Target(map[string]any{"bucket": "fixture", "region": "us-east-1", "endpoint": server.URL, "credentials_ref": `{"access_key_id":"fixture","secret_access_key":"fixture"}`}) // #nosec G101 -- fixture dummy credentials only
	if err != nil {
		t.Fatal(err)
	}
	backup := mustAssert[*S3Target](t, target)
	backup.listingLimits = limits
	if err := backup.InitializeForRead(t.Context()); err != nil {
		t.Fatal(err)
	}
	return backup
}

func TestS3ListingProcessesPagesBeforeFetchingNext(t *testing.T) {
	var processed atomic.Int32
	var requests atomic.Int32
	target := newListingTestS3(t, func(w http.ResponseWriter, r *http.Request) {
		page := requests.Add(1)
		if r.URL.Query().Get("max-keys") != "128" {
			t.Errorf("max-keys=%q", r.URL.Query().Get("max-keys"))
		}
		if processed.Load() != page-1 {
			t.Errorf("fetched page %d before previous page was processed (%d)", page, processed.Load())
		}
		w.Header().Set("Content-Type", "application/xml")
		if page < 4 {
			fmt.Fprintf(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>page%d</NextContinuationToken><Contents><Key>message%d</Key></Contents></ListBucketResult>`, page, page)
		} else {
			fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>message4</Key></Contents></ListBucketResult>`)
		}
	}, backupListingLimits{entries: 3, locatorBytes: 4096})
	err := target.WalkBackupMessages(t.Context(), func(plugins.BackupMessage) error { processed.Add(1); return nil })
	if !errors.Is(err, plugins.ErrBackupListingLimit) || processed.Load() != 3 || requests.Load() != 4 {
		t.Fatalf("processed=%d, requests=%d, error=%v", processed.Load(), requests.Load(), err)
	}
}

func TestS3ListingByteBudgetAndCallbackStop(t *testing.T) {
	for _, mode := range []string{"bytes", "callback", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			var requests atomic.Int32
			target := newListingTestS3(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				fmt.Fprint(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>next</NextContinuationToken><Contents><Key>message1</Key></Contents><Contents><Key>message2</Key></Contents></ListBucketResult>`)
			}, backupListingLimits{entries: 100, locatorBytes: 8})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stop := errors.New("stop callback")
			want := plugins.ErrBackupListingLimit
			if mode == "callback" {
				want = stop
			}
			if mode == "cancel" {
				want = context.Canceled
			}
			calls := 0
			err := target.WalkBackupMessages(ctx, func(plugins.BackupMessage) error {
				calls++
				if mode == "callback" {
					return stop
				}
				if mode == "cancel" {
					cancel()
				}
				return nil
			})
			if !errors.Is(err, want) || requests.Load() != 1 || calls != 1 {
				t.Fatalf("requests=%d,calls=%d,error=%v", requests.Load(), calls, err)
			}
		})
	}
}

type oversizedPageFixture struct {
	t        *testing.T
	mode     string
	requests atomic.Int32
}

func (fixture *oversizedPageFixture) handle(w http.ResponseWriter, _ *http.Request) {
	fixture.requests.Add(1)
	if fixture.mode == "token" {
		fmt.Fprint(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>same</NextContinuationToken></ListBucketResult>`)
		return
	}
	if fixture.mode == "too-many-keys" {
		fixture.writeManyKeys(w)
		return
	}
	fixture.writeOversizedKey(w)
}

func (fixture *oversizedPageFixture) writeManyKeys(w http.ResponseWriter) {
	fmt.Fprint(w, "<ListBucketResult>")
	for range s3ListingPageSize + 1 {
		fmt.Fprint(w, "<Contents><Key>x</Key></Contents>")
	}
	fmt.Fprint(w, "</ListBucketResult>")
}

func (fixture *oversizedPageFixture) writeOversizedKey(w http.ResponseWriter) {
	body := "<ListBucketResult><Contents><Key>" + strings.Repeat("x", maxS3ListingPageBytes) + "</Key></Contents></ListBucketResult>"
	if fixture.mode == "known-length" {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	} else {
		mustAssert[http.Flusher](fixture.t, w).Flush()
	}
	fmt.Fprint(w, body)
}

func (fixture *oversizedPageFixture) check(t *testing.T, err error) {
	t.Helper()
	if fixture.mode == "token" {
		fixture.checkTokenPage(t, err)
		return
	}
	fixture.checkRejectedPage(t, err)
}

func (fixture *oversizedPageFixture) checkTokenPage(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "continuation token") || fixture.requests.Load() != 2 {
		t.Fatalf("token error=%v,requests=%d", err, fixture.requests.Load())
	}
}

func (fixture *oversizedPageFixture) checkRejectedPage(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, plugins.ErrBackupListingLimit) || fixture.requests.Load() != 1 {
		t.Fatalf("page error=%v,requests=%d", err, fixture.requests.Load())
	}
}

func TestS3ListingRejectsOversizedOrNonAdvancingPages(t *testing.T) {
	for _, mode := range []string{"known-length", "chunked", "too-many-keys", "token"} {
		t.Run(mode, func(t *testing.T) {
			fixture := &oversizedPageFixture{t: t, mode: mode}
			target := newListingTestS3(t, fixture.handle, backupListingLimits{})
			calls := 0
			err := target.WalkBackupMessages(t.Context(), func(plugins.BackupMessage) error { calls++; return nil })
			fixture.check(t, err)
			if calls != 0 {
				t.Fatalf("processed %d entries from rejected page", calls)
			}
		})
	}
}

func TestS3ListingTimeoutDoesNotIncludeConsumerWork(t *testing.T) {
	var requests atomic.Int32
	target := newListingTestS3(t, func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			fmt.Fprint(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>next</NextContinuationToken><Contents><Key>first</Key></Contents></ListBucketResult>`)
		} else {
			fmt.Fprint(w, `<ListBucketResult><Contents><Key>second</Key></Contents></ListBucketResult>`)
		}
	}, backupListingLimits{})
	target.operationTimeout = 100 * time.Millisecond
	calls := 0
	err := target.WalkBackupMessages(t.Context(), func(plugins.BackupMessage) error {
		calls++
		if calls == 1 {
			<-time.After(150 * time.Millisecond)
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("consumer work exhausted S3 request timeout: calls=%d, error=%v", calls, err)
	}
}

func TestMaildirListingRejectsSymlinkedRoot(t *testing.T) {
	root := physicalTempDir(t)
	link := filepath.Join(physicalTempDir(t), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	target, err := NewMaildirTarget(map[string]any{"path": link})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mustAssert[*MaildirTarget](t, target).ListBackupMessages(t.Context()); err == nil {
		t.Fatal("listing followed symlinked root")
	}
}

func TestS3ListingSucceedsExactlyAtBudgets(t *testing.T) {
	target := newListingTestS3(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<ListBucketResult><Contents><Key>message1</Key></Contents><Contents><Key>message2</Key></Contents></ListBucketResult>`)
	}, backupListingLimits{entries: 2, locatorBytes: 16})
	entries, err := target.ListBackupMessages(t.Context())
	if err != nil || len(entries) != 2 {
		t.Fatalf("exact boundary: entries=%v, error=%v", entries, err)
	}
}

func TestS3ListingBoundsTotalIOAcrossEmptyPages(t *testing.T) {
	var requests atomic.Int32
	target := newListingTestS3(t, func(w http.ResponseWriter, r *http.Request) {
		page := requests.Add(1)
		select {
		case <-time.After(35 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		fmt.Fprintf(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>page%d</NextContinuationToken></ListBucketResult>`, page)
	}, backupListingLimits{})
	target.operationTimeout = 80 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	err := target.WalkBackupMessages(ctx, func(plugins.BackupMessage) error { t.Fatal("empty page produced a message"); return nil })
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || requests.Load() > 3 {
		t.Fatalf("listing reset its I/O timeout: error=%v, parent=%v, requests=%d", err, ctx.Err(), requests.Load())
	}
}
