package source

import (
	"bufio"
	"fmt"
	"net"
	"testing"
)

func TestPOP3StatLargeMailboxSize(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		command, err := bufio.NewReader(server).ReadString('\n')
		if err == nil && command != "STAT\r\n" {
			err = fmt.Errorf("unexpected command %q", command)
		}
		if err == nil {
			_, err = fmt.Fprint(server, "+OK 2 4294967296\r\n")
		}
		done <- err
	}()
	protocol := &pop3Protocol{conn: client, reader: bufio.NewReader(client)}
	count, size, err := protocol.Stat()
	if serverErr := <-done; serverErr != nil {
		t.Fatal(serverErr)
	}
	if err != nil {
		t.Fatalf("STAT rejected a 4 GiB mailbox containing two messages: %v", err)
	}
	if count != 2 || int64(size) != 4294967296 {
		t.Fatalf("STAT = (%d, %d), want (2, 4294967296)", count, size)
	}
}
