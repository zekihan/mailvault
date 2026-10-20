package sync

import (
	"github.com/zekihan/mailvault/internal/dedup"
	"github.com/zekihan/mailvault/internal/plugins"
)

func targetReceiptScope(target plugins.Target, scope string) string {
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
	if policy, ok := target.(interface{ ReceiptScope() string }); ok && policy.ReceiptScope() != "" {
		return "placement:" + dedup.ComputeContentHash([]byte(policy.ReceiptScope()+"\x00"+identity))
	}
	return identity
}
