package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func serveFolderConnection(conn net.Conn) {
	_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) < 2 {
			break
		}
		folderServerResponseParts(conn, parts)
	}
	_ = conn.Close()
}

func folderServerResponseParts(conn net.Conn, parts []string) {
	switch parts[1] {
	case "LOGIN":
		_, _ = fmt.Fprintf(conn, "%s OK login\r\n", parts[0])
	case "CAPABILITY":
		_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1\r\n%s OK capabilities\r\n", parts[0])
	case "LIST":
		_, _ = fmt.Fprintf(conn, "* LIST (\\Noselect) \"/\" \"Folders\"\r\n* LIST () \"/\" Ar&AV8-iv\r\n* LIST () \"/\" \"Folders/invoice\\\\/receipt\"\r\n%s OK listed\r\n", parts[0])
	default:
		_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", parts[0])
	}
}

func startFolderServer(t *testing.T) (net.Listener, <-chan struct{}) {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 2 {
			conn, e := listener.Accept()
			if e != nil {
				return
			}
			serveFolderConnection(conn)
		}
	}()
	return listener, done
}

func assertFolderOutput(t *testing.T, out *bytes.Buffer, asJSON bool) {
	t.Helper()
	expected := []string{"Arşiv", `Folders/invoice\/receipt`}
	if asJSON {
		var got []string
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, expected) {
			t.Fatalf("got %q", got)
		}
		return
	}
	want := "folders:\n  - \"Arşiv\"\n  - \"Folders/invoice\\\\/receipt\"\n"
	if out.String() != want {
		t.Fatalf("output %q, want %q", out.String(), want)
	}
}

func TestListFoldersDecodesNamesAndIgnoresUnrelatedCredentials(t *testing.T) {
	listener, done := startFolderServer(t)
	defer listener.Close()
	oldConfig, oldJSON := cfgFile, jsonOutput
	defer func() { cfgFile = oldConfig; jsonOutput = oldJSON }()
	cfgFile = filepath.Join(physicalTempDir(t), "config.yaml")
	content := fmt.Sprintf("sources:\n  - name: mailbox\n    type: imap\n    host: 127.0.0.1\n    port: %d\n    use_tls: false\n    username: test\n    password_ref: literal:synthetic\n  - name: unrelated\n    password_ref: file:/does-not-exist\n", mustAssert[*net.TCPAddr](t, listener.Addr()).Port)
	if err := os.WriteFile(cfgFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, asJSON := range []bool{false, true} {
		jsonOutput = asJSON
		cmd := newListFoldersCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"--source", "mailbox"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		assertFolderOutput(t, &out, asJSON)
	}
	<-done
}
