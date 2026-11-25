package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

type streamingReader struct {
	t        *testing.T
	count    int
	reads    int
	dryRun   bool
	failure  error
	appender *fixtureAppender
}

func (reader *streamingReader) ListBackupMessages(context.Context) ([]plugins.BackupMessage, error) {
	reader.t.Fatal("restore materialized the listing instead of walking it")
	return nil, nil
}
func (reader *streamingReader) WalkBackupMessages(ctx context.Context, visit func(plugins.BackupMessage) error) error {
	plans, err := filepath.Glob(filepath.Join(os.Getenv("TMPDIR"), "mailvault-restore-*"))
	wantPlans := 1
	if reader.dryRun {
		wantPlans = 0
	}
	if err != nil || len(plans) != wantPlans {
		reader.t.Fatalf("private plan files=%v, %v", plans, err)
	}
	if len(plans) == 1 {
		info, err := os.Stat(plans[0])
		if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
			reader.t.Fatalf("restore plan permissions: %v, %v", info, err)
		}
	}
	for i := range reader.count {
		if reader.reads != i {
			reader.t.Fatalf("entry %d was not read before advancing (reads=%d)", i, reader.reads)
		}
		if reader.appender != nil && reader.appender.ensured != 0 {
			reader.t.Fatal("destination touched before complete preflight")
		}
		hash := sha256.Sum256([]byte(fmt.Sprintf("raw message %d", i)))
		if err := visit(plugins.BackupMessage{Locator: strconv.Itoa(i), RawSHA256: hex.EncodeToString(hash[:])}); err != nil {
			return err
		}
	}
	return reader.failure
}
func (reader *streamingReader) ReadBackupMessage(_ context.Context, locator string) (*plugins.Message, error) {
	reader.reads++
	return &plugins.Message{Raw: []byte("raw message " + locator)}, nil
}

type streamingModeFixture struct {
	mode     string
	appender *fixtureAppender
	reader   *streamingReader
}

func (fixture *streamingModeFixture) open(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	fixture.appender = &fixtureAppender{}
	fixture.reader = &streamingReader{t: t, count: 300, appender: fixture.appender, dryRun: fixture.mode == "dry-run"}
	if fixture.mode == "over-budget" {
		fixture.reader.failure = plugins.ErrBackupListingLimit
	}
}

func (fixture *streamingModeFixture) run(t *testing.T) (Result, error) {
	t.Helper()
	options := Options{BackupTarget: "backup", DestinationSource: "destination", DestinationFolder: "Restored", DryRun: fixture.mode == "dry-run"}
	return Run(t.Context(), fixture.reader, fixture.appender, newFixtureReceipts(), options)
}

func (fixture *streamingModeFixture) check(t *testing.T, result Result, err error) {
	t.Helper()
	if fixture.mode == "over-budget" {
		fixture.checkOverBudget(t, err)
		return
	}
	fixture.checkCompleted(t, result, err)
}

func (fixture *streamingModeFixture) checkOverBudget(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, plugins.ErrBackupListingLimit) || fixture.appender.ensured != 0 || fixture.appender.appended != 0 {
		t.Fatalf("failed preflight touched destination: %+v, %v", fixture.appender, err)
	}
}

func (fixture *streamingModeFixture) checkCompleted(t *testing.T, result Result, err error) {
	t.Helper()
	if err != nil || result.Messages != 300 {
		t.Fatalf("result=%+v, error=%v", result, err)
	}
	switch fixture.mode {
	case "restore":
		fixture.checkRestored(t, result)
	case "dry-run":
		fixture.checkDryRun(t, result)
	}
}

func (fixture *streamingModeFixture) checkRestored(t *testing.T, result Result) {
	t.Helper()
	if result.Restored != 300 || fixture.reader.reads != 600 {
		t.Fatalf("restore=%+v, reads=%d", result, fixture.reader.reads)
	}
}

func (fixture *streamingModeFixture) checkDryRun(t *testing.T, result Result) {
	t.Helper()
	if result.Planned != 300 || fixture.appender.ensured != 0 {
		t.Fatalf("dry run=%+v, appender=%+v", result, fixture.appender)
	}
}

func TestRestoreStreamsPreflightAndCleansPrivatePlan(t *testing.T) {
	for _, mode := range []string{"restore", "dry-run", "over-budget"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			fixture := &streamingModeFixture{mode: mode}
			fixture.open(t, dir)
			result, err := fixture.run(t)
			fixture.check(t, result, err)
			files, err := filepath.Glob(filepath.Join(dir, "mailvault-restore-*"))
			if err != nil || len(files) != 0 {
				t.Fatalf("restore plan leaked: %v, %v", files, err)
			}
		})
	}
}
