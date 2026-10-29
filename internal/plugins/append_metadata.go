package plugins

import (
	"fmt"
	"strings"
)

// NormalizeAppendFlags validates archive flags for IMAP APPEND and drops Recent,
// which is maintained by the server. System flags are normalized to their wire spelling.
func NormalizeAppendFlags(values []string) ([]string, error) {
	flags := make([]string, 0, len(values))
	for _, value := range values {
		switch strings.ToLower(value) {
		case "\\recent":
			continue
		case "\\seen":
			flags = append(flags, "\\Seen")
			continue
		case "\\answered":
			flags = append(flags, "\\Answered")
			continue
		case "\\flagged":
			flags = append(flags, "\\Flagged")
			continue
		case "\\deleted":
			flags = append(flags, "\\Deleted")
			continue
		case "\\draft":
			flags = append(flags, "\\Draft")
			continue
		}
		if value == "" || strings.HasPrefix(value, "\\") {
			return nil, fmt.Errorf("unsupported IMAP system flag %q", value)
		}
		for _, char := range []byte(value) {
			if char < 0x21 || char > 0x7e || strings.ContainsRune("(){%*\\\"]", rune(char)) {
				return nil, fmt.Errorf("invalid IMAP keyword flag %q", value)
			}
		}
		flags = append(flags, value)
	}
	return flags, nil
}
