package memory_test

import (
	"testing"

	"github.com/Senjumarru/miniwallet/internal/service"
	"github.com/Senjumarru/miniwallet/internal/storage/memory"
)

// Статическая проверка соответствия конкретных типов репозиториев памяти интерфейсам service
var (
	_ service.UserRepository          = (*memory.UserRepo)(nil)
	_ service.OrderRepository         = (*memory.OrderRepo)(nil)
	_ service.PaymentRepository       = (*memory.PaymentRepo)(nil)
	_ service.WebhookEventRepository  = (*memory.WebhookRepo)(nil)
	_ service.PaymentEventRepository  = (*memory.PaymentEventRepo)(nil)
	_ service.SecurityEventRepository = (*memory.SecurityEventRepo)(nil)
	_ service.TxManager               = (*memory.Storage)(nil)
)

func TestMemory_ImplementsServiceInterfaces(t *testing.T) {
	store := memory.New()

	var _ service.UserRepository = store.Users()
	var _ service.OrderRepository = store.Orders()
	var _ service.PaymentRepository = store.Payments()
	var _ service.WebhookEventRepository = store.Webhooks()
	var _ service.PaymentEventRepository = store.PaymentEvents()
	var _ service.SecurityEventRepository = store.SecurityEvents()
	var _ service.TxManager = store
}
