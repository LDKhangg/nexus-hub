package service

type TextSanitizer struct {
	MaxTokensLimit int
}

func NewTextSanitizer(maxLimit int) *TextSanitizer {
	return &TextSanitizer{
		MaxTokensLimit: maxLimit,
	}
}

func (f *TextSanitizer) OptimizeTokens(rawContent string) string {
	// OptimizeTokens
	if rawContent == "" {
		return ""
	}
	// html/md stripping

	// noise filtering
	// token budgeting
	return ""
}

func removeHTMLTags(content string) string {
	return ""
}

func normalizeWhitespace(content string) string {
	return ""
}
