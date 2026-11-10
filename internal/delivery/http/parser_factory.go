package http

import (
	"errors"
	"net/http"

	"github.com/LDKhangg/nexus-hub/internal/adapters/sources/clickup"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

var ErrUnknownSource = errors.New("unknown webhook source")

type EventParser interface {
	Parser(rawBody []byte) ([]domain.NormalizedEvent, error)
}

// SignatureVerifier authenticates the exact body bytes before parsing.
type SignatureVerifier interface {
	Verify(header http.Header, rawBody []byte) error
}

type VerifiedEventParser interface {
	EventParser
	SignatureVerifier
}
type VerifiedParserFactory interface {
	ForIntegration(tenant.WebhookIntegration) (VerifiedEventParser, error)
}
type IntegrationParserFactory struct{ lookupEnv func(string) (string, bool) }

func NewVerifiedParserFactory(lookupEnv func(string) (string, bool)) *IntegrationParserFactory {
	return &IntegrationParserFactory{lookupEnv: lookupEnv}
}
func (f *IntegrationParserFactory) ForIntegration(integration tenant.WebhookIntegration) (VerifiedEventParser, error) {
	if err := tenant.ValidateClickUpSource(integration); err != nil {
		return nil, ErrUnknownSource
	}
	cfg, err := tenant.ParseClickUpNoticeConfig(integration.Config)
	if err != nil {
		return nil, err
	}
	if f.lookupEnv == nil {
		return nil, errors.New("webhook secret unavailable")
	}
	secret, ok := f.lookupEnv(cfg.WebhookSecretEnv)
	if !ok || secret == "" {
		return nil, errors.New("webhook secret unavailable")
	}
	return clickup.NewClickUpParser(secret, cfg.OwnSenderID), nil
}

type WebhookFactory struct {
	parsers map[domain.ChannelSource]EventParser
}

func NewWebhookFactory() *WebhookFactory {
	return &WebhookFactory{parsers: make(map[domain.ChannelSource]EventParser)}
}

func (f *WebhookFactory) Register(source domain.ChannelSource, parser EventParser) {
	f.parsers[source] = parser
}

func (f *WebhookFactory) GetParser(source domain.ChannelSource) (EventParser, error) {
	parser, ok := f.parsers[source]
	if !ok {
		return nil, ErrUnknownSource
	}
	return parser, nil
}
