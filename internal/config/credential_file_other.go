//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package config

import (
	"fmt"
	"os"
)

func openCredentialFile(string) (*os.File, error) {
	return nil, fmt.Errorf("safe credential file references are unsupported on this platform; use env: or keyring:")
}

func validateCredentialFile(os.FileInfo) error {
	return fmt.Errorf("safe credential file references are unsupported on this platform")
}
