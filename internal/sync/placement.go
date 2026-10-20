package sync

import (
	"strings"

	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
)

func targetReceiptScope(target plugins.Target, scope string) string {
	if identity, ok := target.(plugins.TargetDestinationIdentity); ok && identity.DestinationIdentity() != "" {
		scope = "destination:" + identity.DestinationIdentity() + ":" + scope
	}
	if policy, ok := target.(interface{ ReceiptScope() string }); ok && policy.ReceiptScope() != "" {
		return "placement:" + dedup.ComputeContentHash([]byte(scope+"\x00"+policy.ReceiptScope()))
	}
	return scope
}

func targetDeliveryIdentity(target plugins.Target, msg *plugins.Message, identity string) string {
	if policy, ok := target.(interface {
		DeliveryIdentity(*plugins.Message, string) string
	}); ok {
		identity = policy.DeliveryIdentity(msg, identity)
	}
	var scopes []string
	if policy, ok := target.(interface{ ReceiptScope() string }); ok && policy.ReceiptScope() != "" {
		scopes = append(scopes, policy.ReceiptScope())
	}
	if destination, ok := target.(plugins.TargetDestinationIdentity); ok && destination.DestinationIdentity() != "" {
		scopes = append(scopes, destination.DestinationIdentity())
	}
	if len(scopes) > 0 {
		return "placement:" + dedup.ComputeContentHash([]byte(strings.Join(scopes, "\x00")+"\x00"+identity))
	}
	return identity
}
