//go:build unix

package target

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMboxRejectsNamedPipe(t *testing.T) {
	for _, operation := range []string{"initialize", "list", "read"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "archive.mbox")
			if err := unix.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
			// Keep both ends open so a blocking open in the old code completes
			// deterministically. No timing assumptions or background goroutines.
			keeper, err := os.OpenFile(path, os.O_RDWR|unix.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer keeper.Close()
			mbox := &MboxTarget{path: path}
			defer mbox.Close()
			switch operation {
			case "initialize":
				err = mbox.Initialize(context.Background())
			case "list":
				_, err = mbox.ListBackupMessages(context.Background())
			case "read":
				_, err = mbox.ReadBackupMessage(context.Background(), "0:1:0:0")
			}
			if err == nil || !strings.Contains(err.Error(), "regular file") {
				t.Fatalf("%s FIFO error = %v, want regular file rejection", operation, err)
			}
		})
	}
}
