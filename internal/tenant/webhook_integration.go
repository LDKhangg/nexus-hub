package tenant

type WebhookIntegration struct {
	ID          string
	WorkspaceID string
	Platform    string
	Role        IntegrationRole
	IsEnabled   bool
	Config      map[string]any
}
