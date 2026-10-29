package service

import (
	"context"

	"github.com/LDKhangg/nexus-hub/internal/relay"
)

type Processor struct{}

func NewProcessor(sanitizer *TextSanitizer) *Processor {
	return &Processor{}
}

func (f *Processor) HandleFLush(ctx context.Context, event relay.NormalizedEvent) error {
	return nil
}
