//go:build !unix && !windows

package target

import (
	"errors"
	"os"
)

const maildirReadFlags = os.O_RDONLY

func linkMaildirFile(_ *maildirDirectory, _ string, _ *maildirDirectory, _ string) error {
	// Do not fall back to pathname-based publication on unsupported hosts.
	return errors.New("secure Maildir publication requires descriptor-relative hard links on this host")
}

func openAbsoluteMaildir(_ string, _ bool, _ *[]maildirCreation) (*maildirDirectory, error) {
	return nil, errors.New("secure Maildir access requires directory descriptors on this host")
}
