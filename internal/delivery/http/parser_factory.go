package http

import (
	"fmt"

	"github.com/LDKhangg/nexus-hub/internal/relay"
)

type EventParser interface {
	Parser(rawBody []byte) (relay.NormalizedEvent, error)
}

type WebhookFactory struct {
	parsers map[relay.ChannelSource]EventParser
}

func NewWebhookFactory() *WebhookFactory {
	return &WebhookFactory{
		parsers: make(map[relay.ChannelSource]EventParser),
	}
}

func (f *WebhookFactory) Register(source relay.ChannelSource, parser EventParser) {
	f.parsers[source] = parser
}

func (f *WebhookFactory) GetParser(source relay.ChannelSource) (EventParser, error) {
	parser, exists := f.parsers[source]
	if !exists {
		return nil, fmt.Errorf("cant find parser factory")
	}
	return parser, nil
}
