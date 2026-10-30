package http

import (
	"fmt"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

type EventParser interface {
	Parser(rawBody []byte) (domain.NormalizedEvent, error)
}

type WebhookFactory struct {
	parsers map[domain.ChannelSource]EventParser
}

func NewWebhookFactory() *WebhookFactory {
	return &WebhookFactory{
		parsers: make(map[domain.ChannelSource]EventParser),
	}
}

func (f *WebhookFactory) Register(source domain.ChannelSource, parser EventParser) {
	f.parsers[source] = parser
}

func (f *WebhookFactory) GetParser(source domain.ChannelSource) (EventParser, error) {
	parser, exists := f.parsers[source]
	if !exists {
		return nil, fmt.Errorf("cant find parser factory")
	}
	return parser, nil
}
