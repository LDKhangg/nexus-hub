package relay

import "github.com/LDKhangg/nexus-hub/internal/domain"

type DropReason string

const DropOwnBot DropReason = "own_bot"

func shouldDrop(e domain.NormalizedEvent) (bool, DropReason) {
	if e.FromOwnBot {
		return true, DropOwnBot
	}
	return false, ""
}
