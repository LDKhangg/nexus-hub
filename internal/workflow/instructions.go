package workflow

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	// OwnerInstructionsMaxBytes bounds the complete prompt block.
	OwnerInstructionsMaxBytes = 8 * 1024

	// CheckpointOwnerInstructions records the highest instruction version read
	// by a model or investigation attempt. ReferenceID is the decimal version.
	CheckpointOwnerInstructions = "owner_instructions"
)

// InstructionReader is the narrow read side needed at workflow checkpoints.
type InstructionReader interface {
	ListInstructions(context.Context, string, int64) ([]Instruction, error)
}

// OwnerInstructionsUsedVersion returns the highest instruction version
// referenced by a persisted step checkpoint.
func OwnerInstructionsUsedVersion(steps []Step) int64 {
	var usedVersion int64
	for _, step := range steps {
		if step.Checkpoint == nil {
			continue
		}
		for _, reference := range step.Checkpoint.References {
			if reference.Kind != CheckpointOwnerInstructions {
				continue
			}
			version, err := strconv.ParseInt(reference.ReferenceID, 10, 64)
			if err == nil && version > usedVersion {
				usedVersion = version
			}
		}
	}
	return usedVersion
}

// FormatOwnerInstructions renders owner guidance as a separate prompt block.
// It keeps the newest updates when the complete block would exceed maxBytes,
// while preserving chronological order among the updates that remain.
func FormatOwnerInstructions(instructions []Instruction, maxBytes int) (string, int64) {
	if len(instructions) == 0 || maxBytes <= 0 {
		return "", 0
	}
	ordered := append([]Instruction(nil), instructions...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Version == ordered[j].Version {
			return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
		}
		return ordered[i].Version < ordered[j].Version
	})

	const header = "<owner_instructions>\n"
	const footer = "</owner_instructions>\n"
	const omitted = "older updates omitted\n"
	available := maxBytes - len(header) - len(footer)
	if available <= 0 {
		return "", 0
	}

	lines := make([]string, 0, len(ordered))
	for _, instruction := range ordered {
		text := strings.Join(strings.Fields(strings.ToValidUTF8(instruction.Text, "")), " ")
		text = strings.NewReplacer("<", "‹", ">", "›").Replace(text)
		lines = append(lines, fmt.Sprintf("v%d (%s): %s\n", instruction.Version, instruction.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"), text))
	}

	start := len(lines)
	remaining := available
	for i := len(lines) - 1; i >= 0; i-- {
		reserve := 0
		if i > 0 {
			reserve = len(omitted)
		}
		if len(lines[i])+reserve > remaining {
			break
		}
		start = i
		remaining -= len(lines[i])
	}
	omittedOlder := start > 0
	if start == len(lines) {
		reserve := 0
		if len(lines) > 1 {
			reserve = len(omitted)
		}
		line := clipInstructionLine(lines[len(lines)-1], available-reserve)
		if line == "" {
			return "", 0
		}
		lines = []string{line}
		start = 0
		omittedOlder = reserve > 0
	}

	var b strings.Builder
	b.Grow(maxBytes)
	b.WriteString(header)
	if omittedOlder {
		b.WriteString(omitted)
	}
	for _, line := range lines[start:] {
		b.WriteString(line)
	}
	b.WriteString(footer)
	return b.String(), ordered[len(ordered)-1].Version
}

func clipInstructionLine(line string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(line) <= maxBytes {
		return line
	}
	line = strings.TrimSuffix(line, "\n")
	const suffix = "…\n"
	if maxBytes <= len(suffix) {
		return ""
	}
	line = line[:maxBytes-len(suffix)]
	line = strings.ToValidUTF8(line, "")
	return line + suffix
}
