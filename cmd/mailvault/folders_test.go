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

func TestListFoldersDecodesNamesAndIgnoresUnrelatedCredentials(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2; i++ {
			conn, e := listener.Accept()
			if e != nil {
				return
			}
			_, _ = fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
			scanner := bufio.NewScanner(conn)
			for scanner.Scan() {
				p := strings.Fields(scanner.Text())
				if len(p) < 2 {
					break
				}
				switch p[1] {
				case "LOGIN":
					_, _ = fmt.Fprintf(conn, "%s OK login\r\n", p[0])
				case "CAPABILITY":
					_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1\r\n%s OK capabilities\r\n", p[0])
				case "LIST":
					_, _ = fmt.Fprintf(conn, "* LIST (\\Noselect) \"/\" \"Folders\"\r\n* LIST () \"/\" Ar&AV8-iv\r\n* LIST () \"/\" \"Folders/invoice\\\\/receipt\"\r\n%s OK listed\r\n", p[0])
				default:
					_, _ = fmt.Fprintf(conn, "%s BAD unexpected\r\n", p[0])
				}
			}
			_ = conn.Close()
		}
	}()
	oldConfig, oldJSON := cfgFile, jsonOutput
	defer func() { cfgFile = oldConfig; jsonOutput = oldJSON }()
	cfgFile = filepath.Join(t.TempDir(), "config.yaml")
	content := fmt.Sprintf("sources:\n  - name: mailbox\n    type: imap\n    host: 127.0.0.1\n    port: %d\n    use_tls: false\n    username: test\n    password_ref: literal:synthetic\n  - name: unrelated\n    password_ref: file:/does-not-exist\n", listener.Addr().(*net.TCPAddr).Port)
	if err := os.WriteFile(cfgFile, []byte(content), 0600); err != nil {
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
		expected := []string{"Arşiv", `Folders/invoice\/receipt`}
		if asJSON {
			var got []string
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, expected) {
				t.Fatalf("got %q", got)
			}
		} else {
			want := "folders:\n  - \"Arşiv\"\n  - \"Folders/invoice\\\\/receipt\"\n"
			if out.String() != want {
				t.Fatalf("output %q, want %q", out.String(), want)
			}
		}
	}
	<-done
}
