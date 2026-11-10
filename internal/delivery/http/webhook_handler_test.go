package http

import (
	"context"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

type integrationLookupStub struct{}

func (integrationLookupStub) LookupWebhookIntegration(context.Context, string) (tenant.WebhookIntegration, error) {
	return tenant.WebhookIntegration{}, nil
}

func TestWebhookHandlerAcceptsInterfaceDependency(t *testing.T) {
	lookup := integrationLookupStub{}
	handler := NewWebhookHandler(NewWebhookFactory(), nil, lookup)
	if handler.integrations != lookup {
		t.Fatal("handler did not retain the integration lookup dependency")
	}
}
