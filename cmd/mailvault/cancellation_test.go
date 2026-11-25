package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestSyncHonorsCommandCancellation(t *testing.T) {
	configureCLI(t)
	jsonOutput, dryRun = true, true
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	output, err := captureOutput(t, func() error { return runSync(cmd, nil) })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sync ignored cancellation: %v", err)
	}
	if output != "" {
		t.Fatalf("sync canceled before startup wrote a report: %q", output)
	}
}

func TestJSONSyncCancellationRetainsReport(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	configureCLIOnPort(t, mustAssert[*net.TCPAddr](t, listener.Addr()).Port, "10s")
	jsonOutput, dryRun = true, true

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		cancel()
		<-done
	}()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	output, err := captureOutput(t, func() error { return runSync(cmd, nil) })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted sync error = %v, want context cancellation", err)
	}
	var report struct {
		TotalErrors int `json:"TotalErrors"`
		Results     []struct {
			SourceName string `json:"SourceName"`
			TargetName string `json:"TargetName"`
		} `json:"Results"`
	}
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("interrupted sync stdout = %q, want a JSON report: %v", output, err)
	}
	if report.TotalErrors == 0 || len(report.Results) != 1 || report.Results[0].SourceName != "test-account" || report.Results[0].TargetName != "local" {
		t.Fatalf("interrupted sync lost its pair details: %s", output)
	}
}
