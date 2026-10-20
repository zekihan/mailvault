//go:build linux

package target

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestMboxRollsBackPartialAppend(t *testing.T) {
	if os.Getenv("MAILVAULT_PARTIAL_APPEND_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestMboxRollsBackPartialAppend$", "-test.v")
		cmd.Env = append(os.Environ(), "MAILVAULT_PARTIAL_APPEND_CHILD=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("partial append regression: %v\n%s", err, output)
		}
		return
	}
	path := filepath.Join(t.TempDir(), "mailbox")
	target := &MboxTarget{path: path, create: true}
	if err := target.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	original := []byte("From existing\nSubject: existing\n\nold body")
	if _, err := target.file.Write(original); err != nil {
		t.Fatal(err)
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 256, Max: limit.Max}); err != nil {
		t.Fatal(err)
	}
	msg := &plugins.Message{Raw: []byte("Subject: synthetic\n\n" + strings.Repeat("x", 1500) + "\n")}
	writeErr := target.WriteMessage(context.Background(), msg)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	if writeErr == nil {
		t.Fatal("expected forced append failure")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, original) {
		t.Fatalf("failed append left %d bytes; want original %d", len(data), len(original))
	}
	if err := target.WriteMessage(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(data, []byte("From MAILER-DAEMON ")) != 1 {
		t.Fatal("retry created duplicate separator")
	}
	if !bytes.HasPrefix(data, original) || !bytes.HasSuffix(data, append(msg.Raw, '\n')) {
		t.Fatal("retry damaged old or new contents")
	}
}
