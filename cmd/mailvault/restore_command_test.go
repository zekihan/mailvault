package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/target"
	"github.com/zekihan/mailvault/internal/testutil/mailtest"
)

func restoreIMAPCommand(conn net.Conn, parts []string, listCalls, unexpectedCalls *atomic.Int32) {
	switch parts[1] {
	case "LOGIN":
		_, _ = fmt.Fprintf(conn, "%s OK login\r\n", parts[0])
	case "CAPABILITY":
		_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1\r\n%s OK capabilities\r\n", parts[0])
	case "LIST":
		listCalls.Add(1)
		_, _ = fmt.Fprintf(conn, "* LIST () \"/\" \"Restored\"\r\n%s OK listed\r\n", parts[0])
	default:
		unexpectedCalls.Add(1)
		_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", parts[0])
	}
}

func serveRestoreIMAP(conn net.Conn, listCalls, unexpectedCalls *atomic.Int32) {
	_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) < 2 {
			return
		}
		restoreIMAPCommand(conn, parts, listCalls, unexpectedCalls)
	}
}

func setupRestoreMboxFixture(t *testing.T, port int) string {
	t.Helper()
	configureCLIOnPort(t, port, "3s")
	jsonOutput = true
	archive := filepath.Join(physicalTempDir(t), "archive.mbox")
	contents := fmt.Sprintf(`sources:
  - name: destination
    type: imap
    host: 127.0.0.1
    port: %d
    username: fixture
    password_ref: env:MAILVAULT_TEST_RESTORE_PASSWORD
    use_tls: false
    connection_timeout: 3s
    read_timeout: 3s
targets:
  - name: archive
    type: mbox
    config:
      path: %q
pairs:
  - source: destination
    targets: [archive]
`, port, archive)
	if err := os.WriteFile(cfgFile, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	mbox, err := target.NewMboxTarget(map[string]any{"path": archive})
	if err != nil {
		t.Fatal(err)
	}
	if err := mbox.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	writeErr := mbox.WriteMessage(t.Context(), &plugins.Message{Raw: []byte("Subject: fixture\n\nmessage body\n")})
	if err := errors.Join(writeErr, mbox.Close()); err != nil {
		t.Fatal(err)
	}
	return archive
}

func assertRestorePreview(t *testing.T, args []string, listCalls, unexpectedCalls *atomic.Int32) {
	t.Helper()
	t.Setenv("MAILVAULT_TEST_RESTORE_PASSWORD", "")
	missingTemp := filepath.Join(stateDir, "missing")
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, missingTemp)
	}
	output, err := executeBackupCommand(t, newRestoreCommand(), append(args, "--dry-run")...)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Mode       string `json:"mode"`
		Messages   int    `json:"messages"`
		Planned    int    `json:"planned"`
		Restored   int    `json:"restored"`
		Unverified int    `json:"unverified"`
	}
	if err := json.Unmarshal(output, &got); err != nil || got.Mode != "dry-run" || got.Messages != 1 || got.Planned != 1 || got.Restored != 0 || got.Unverified != 1 {
		t.Fatalf("restore preview = %s, decode error = %v", output, err)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil || len(entries) != 0 || listCalls.Load() != 0 || unexpectedCalls.Load() != 0 {
		t.Fatalf("restore preview touched state or mailbox: entries = %v, error = %v", entries, err)
	}
}

func assertRestoreExistingRefusal(t *testing.T, args []string, listCalls, unexpectedCalls *atomic.Int32) {
	t.Helper()
	t.Setenv("MAILVAULT_TEST_RESTORE_PASSWORD", "synthetic-restore-value")
	for range 2 {
		_, err := executeBackupCommand(t, newRestoreCommand(), append(args, "--allow-unverified")...)
		if !errors.Is(err, plugins.ErrMailboxExists) {
			t.Fatalf("restore into existing mailbox: %v", err)
		}
	}
	if listCalls.Load() != 2 || unexpectedCalls.Load() != 0 {
		t.Fatalf("expected only mailbox listings, got %d listings and %d unexpected commands", listCalls.Load(), unexpectedCalls.Load())
	}
}

func TestRestoreMboxDryRunAndExistingFolderRefusal(t *testing.T) {
	var listCalls, unexpectedCalls atomic.Int32
	_, port := mailtest.ProtocolServer(t, func(conn net.Conn, _ <-chan struct{}) {
		serveRestoreIMAP(conn, &listCalls, &unexpectedCalls)
	})
	archive := setupRestoreMboxFixture(t, port)
	before := readCommandFixture(t, archive)
	args := []string{"--from", "archive", "--source", "destination", "--folder", "Restored"}
	t.Run("preview needs no writable scratch space", func(t *testing.T) {
		assertRestorePreview(t, args, &listCalls, &unexpectedCalls)
	})
	assertRestoreExistingRefusal(t, args, &listCalls, &unexpectedCalls)
	if !bytes.Equal(before, readCommandFixture(t, archive)) {
		t.Fatal("restore changed the backup")
	}
}
