package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseBinaryReportsLinkedVersion(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mailvault")
	build := exec.Command("go", "build", "-ldflags", "-X main.version=v9.8.7-test", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build release binary: %v\n%s", err, output)
	}
	output, err := exec.Command(binary, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("version command: %v\n%s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != "mailvault version v9.8.7-test" {
		t.Fatalf("version output=%q", got)
	}
}
