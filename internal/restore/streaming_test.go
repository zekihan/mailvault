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
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

type streamingReader struct {
	t        *testing.T
	count    int
	reads    int
	failure  error
	appender *fixtureAppender
}

func (r *streamingReader) ListBackupMessages(context.Context) ([]plugins.BackupMessage, error) {
	r.t.Fatal("restore materialized the listing instead of walking it")
	return nil, nil
}
func (r *streamingReader) WalkBackupMessages(ctx context.Context, visit func(plugins.BackupMessage) error) error {
	plans, err := filepath.Glob(filepath.Join(os.Getenv("TMPDIR"), "mailvault-restore-*"))
	if err != nil || len(plans) != 1 {
		r.t.Fatalf("private plan files=%v, %v", plans, err)
	}
	info, err := os.Stat(plans[0])
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		r.t.Fatalf("restore plan permissions: %v, %v", info, err)
	}
	for i := 0; i < r.count; i++ {
		if r.reads != i {
			r.t.Fatalf("entry %d was not read before advancing (reads=%d)", i, r.reads)
		}
		if r.appender != nil && r.appender.ensured != 0 {
			r.t.Fatal("destination touched before complete preflight")
		}
		// Metadata spool contains hashes and identities, never raw mail bytes.
		hash := sha256.Sum256([]byte(fmt.Sprintf("raw message %d", i)))
		if err := visit(plugins.BackupMessage{Locator: fmt.Sprint(i), RawSHA256: hex.EncodeToString(hash[:])}); err != nil {
			return err
		}
	}
	return r.failure
}
func (r *streamingReader) ReadBackupMessage(_ context.Context, locator string) (*plugins.Message, error) {
	r.reads++
	return &plugins.Message{Raw: []byte("raw message " + locator)}, nil
}

func TestRestoreStreamsPreflightAndCleansPrivatePlan(t *testing.T) {
	for _, mode := range []string{"restore", "dry-run", "over-budget"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)
			t.Setenv("TMP", dir)
			t.Setenv("TEMP", dir)
			appender := &fixtureAppender{}
			reader := &streamingReader{t: t, count: 300, appender: appender}
			if mode == "over-budget" {
				reader.failure = plugins.ErrBackupListingLimit
			}
			options := Options{BackupTarget: "backup", DestinationSource: "destination", DestinationFolder: "Restored", DryRun: mode == "dry-run"}
			result, err := Run(context.Background(), reader, appender, newFixtureReceipts(), options)
			if mode == "over-budget" {
				if !errors.Is(err, plugins.ErrBackupListingLimit) || appender.ensured != 0 || appender.appended != 0 {
					t.Fatalf("failed preflight touched destination: %+v, %v", appender, err)
				}
			} else {
				if err != nil || result.Messages != 300 {
					t.Fatalf("result=%+v, error=%v", result, err)
				}
				if mode == "restore" && (result.Restored != 300 || reader.reads != 600) {
					t.Fatalf("restore=%+v, reads=%d", result, reader.reads)
				}
				if mode == "dry-run" && (result.Planned != 300 || appender.ensured != 0) {
					t.Fatalf("dry run=%+v, appender=%+v", result, appender)
				}
			}
			files, err := filepath.Glob(filepath.Join(dir, "mailvault-restore-*"))
			if err != nil || len(files) != 0 {
				t.Fatalf("restore plan leaked: %v, %v", files, err)
			}
		})
	}
}
