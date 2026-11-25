//go:build linux

package target

import (
	"bytes"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func runMboxPartialAppendChild(t *testing.T) bool {
	t.Helper()
	if os.Getenv("MAILVAULT_PARTIAL_APPEND_CHILD") == "1" {
		return false
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestMboxRollsBackPartialAppend$", "-test.v") // #nosec G204,G702 -- self-spawned test binary; argv fully controlled
	cmd.Env = append(os.Environ(), "MAILVAULT_PARTIAL_APPEND_CHILD=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("partial append regression: %v\n%s", err, output)
	}
	return true
}

type mboxRollbackFixture struct {
	target   *MboxTarget
	path     string
	original []byte
	msg      *plugins.Message
	limit    syscall.Rlimit
}

func (fixture *mboxRollbackFixture) open(t *testing.T) {
	t.Helper()
	fixture.path = filepath.Join(t.TempDir(), "mailbox")
	fixture.target = &MboxTarget{path: fixture.path, create: true}
	if err := fixture.target.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	fixture.original = []byte("From existing\nSubject: existing\n\nold body")
	if _, err := fixture.target.file.Write(fixture.original); err != nil {
		t.Fatal(err)
	}
	fixture.msg = &plugins.Message{Raw: []byte("Subject: synthetic\n\n" + strings.Repeat("x", 1500) + "\n")}
}

func (fixture *mboxRollbackFixture) applyLimit(t *testing.T) {
	t.Helper()
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &fixture.limit); err != nil {
		t.Fatal(err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	t.Cleanup(func() { signal.Reset(syscall.SIGXFSZ) })
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 256, Max: fixture.limit.Max}); err != nil {
		t.Fatal(err)
	}
}

func (fixture *mboxRollbackFixture) attemptOversizedWrite(t *testing.T) {
	t.Helper()
	writeErr := fixture.target.WriteMessage(t.Context(), fixture.msg)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &fixture.limit); err != nil {
		t.Fatal(err)
	}
	if writeErr == nil {
		t.Fatal("expected forced append failure")
	}
	data, err := os.ReadFile(fixture.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, fixture.original) {
		t.Fatalf("failed append left %d bytes; want original %d", len(data), len(fixture.original))
	}
}

func (fixture *mboxRollbackFixture) checkRetry(t *testing.T) {
	t.Helper()
	if err := fixture.target.WriteMessage(t.Context(), fixture.msg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(fixture.path)
	if err != nil {
		t.Fatal(err)
	}
	checkMboxRetryData(t, data, fixture.original, fixture.msg)
}

func checkMboxRetryData(t *testing.T, data, original []byte, msg *plugins.Message) {
	t.Helper()
	if bytes.Count(data, []byte("From MAILER-DAEMON ")) != 1 {
		t.Fatal("retry created duplicate separator")
	}
	if !bytes.HasPrefix(data, original) || !bytes.HasSuffix(data, append(msg.Raw, '\n')) {
		t.Fatal("retry damaged old or new contents")
	}
}

func TestMboxRollsBackPartialAppend(t *testing.T) {
	if runMboxPartialAppendChild(t) {
		return
	}
	fixture := &mboxRollbackFixture{}
	fixture.open(t)
	defer fixture.target.Close()
	fixture.applyLimit(t)
	fixture.attemptOversizedWrite(t)
	fixture.checkRetry(t)
}
