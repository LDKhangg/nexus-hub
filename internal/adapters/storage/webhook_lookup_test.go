package storage

import (
	"errors"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"testing"
)

func TestWebhookLookupConfig(t *testing.T) {
	ctx, pool := testDB(t)
	seedThreads(t, ctx, pool)
	r, _ := NewIntegrationRepository(pool)
	mustExec(t, ctx, pool, `UPDATE integrations SET config='{"webhook_id":"w1"}' WHERE id=$1`, integrationA)
	got, err := r.LookupWebhookIntegration(ctx, integrationA)
	if err != nil || got.Config["webhook_id"] != "w1" {
		t.Fatalf("lookup %v %v", got, err)
	}
	if _, err := r.LookupWebhookIntegration(ctx, missingID); !errors.Is(err, tenant.ErrIntegrationNotFound) {
		t.Fatalf("missing %v", err)
	}
	if _, err := r.LookupWebhookIntegration(ctx, "invalid"); err == nil {
		t.Fatal("invalid ID accepted")
	}
}
