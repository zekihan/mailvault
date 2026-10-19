package target

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
)

func TestMboxTargetEscapesFromLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mailbox")
	target := &MboxTarget{path: path, create: true}
	if err := target.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer target.Close()

	body := "From ordinary\r\n>From escaped\n>>From escaped twice\r\n>>>From already escaped\nlast-no-newline"
	if err := target.WriteMessage(context.Background(), &plugins.Message{Raw: []byte(body)}); err != nil {
		t.Fatal(err)
	}
	gotBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(gotBytes)
	if !strings.HasSuffix(got, ">>>>From already escaped\nlast-no-newline\n\n") {
		t.Fatalf("mbox body suffix = %q", got)
	}
	if !strings.Contains(got, "\n>From ordinary\r\n>>From escaped\n>>>From escaped twice\r\n>>>>From already escaped\n") {
		t.Fatalf("mbox body did not mboxrd-escape lines preserving line endings: %q", got)
	}
}

func TestMboxrdUsesFirstHeaderBoundary(t *testing.T) {
	raw := []byte("Subject: test\r\n\r\nFrom forged\r\n\n\nFrom later\n")
	want := "Subject: test\r\n\r\n>From forged\r\n\n\n>From later\n"
	if got := string(escapeMboxrd(raw)); got != want {
		t.Fatalf("escaped body = %q, want %q", got, want)
	}
}

func TestMboxTargetEmptyAndCRLF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mailbox")
	target := &MboxTarget{path: path, create: true}
	if err := target.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	for _, raw := range [][]byte{[]byte("A: b\r\n\r\nFrom x\r\n"), {}} {
		if err := target.WriteMessage(context.Background(), &plugins.Message{Raw: raw}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "\r\n\r\n>From x\r\n\n") {
		t.Fatalf("CRLF payload not preserved: %q", got)
	}
	if !strings.HasSuffix(string(got), "\n\n") {
		t.Fatalf("empty message separator missing: %q", got)
	}
}
