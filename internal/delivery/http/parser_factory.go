package http

import (
	"errors"
	"net/http"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

var ErrUnknownSource = errors.New("unknown webhook source")

type EventParser interface {
	Parser(rawBody []byte) (domain.NormalizedEvent, error)
}

// SignatureVerifier là interface tuỳ chọn: parser nào implement thì handler sẽ verify trước khi parse.
type SignatureVerifier interface {
	Verify(header http.Header, rawBody []byte) error
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
