package config

import (
	"errors"
	"strings"
)

var errSourceCredentialLineBreak = errors.New("source password must be a single line")

// Source password files permit one ordinary text-file terminator. Do not trim
// spaces or normalize env/literal values; reject protocol-breaking line breaks.
// Generic credential file reads remain byte-preserving for multiline S3 JSON.
func resolveSourceCredentialRefAt(reference, configDir string) (string, error) {
	value, err := resolveCredentialRefAt(reference, configDir)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(reference, "file:") {
		if strings.HasSuffix(value, "\r\n") {
			value = strings.TrimSuffix(value, "\r\n")
		} else {
			value = strings.TrimSuffix(value, "\n")
		}
	}
	if strings.ContainsAny(value, "\r\n") {
		return "", errSourceCredentialLineBreak
	}
	return value, nil
}
