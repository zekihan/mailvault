package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateSourcePortRange(t *testing.T) {
	for _, kind := range []string{"imap", "pop3"} {
		for _, port := range []int{-1, 0, 1, 110, 993, 65535, 65536} {
			t.Run(fmt.Sprintf("%s/%d", kind, port), func(t *testing.T) {
				cfg := &Config{Sources: []SourceConfig{{Name: "fixture", Type: kind, Host: "127.0.0.1", Port: port, Username: "fixture", PasswordRef: "synthetic-value", MaxConnections: 1}}, Targets: []TargetConfig{{Name: "archive", Type: "mbox"}}, Pairs: []PairConfig{{Source: "fixture", Targets: []string{"archive"}}}}
				err := Validate(cfg)
				if port < 1 || port > 65535 {
					if err == nil || !strings.Contains(err.Error(), "port") {
						t.Fatalf("invalid port accepted or wrong error: %v", err)
					}
				} else if err != nil {
					t.Fatalf("valid port rejected: %v", err)
				}
			})
		}
	}
}
