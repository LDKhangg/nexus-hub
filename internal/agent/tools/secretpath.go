package tools

import "strings"

// DeniedPath reports whether a repository-relative path may hold secrets.
// It is the single secret-path policy shared by every tool: such files are
// never read, listed, committed or shown in a patch.
func DeniedPath(p string) bool {
	lower := strings.ToLower(p)
	parts := strings.Split(lower, "/")
	for _, part := range parts[:len(parts)-1] {
		if part == "secrets" {
			return true
		}
	}
	base := parts[len(parts)-1]
	switch {
	case base == ".env" || strings.HasPrefix(base, ".env."):
		return true
	case strings.HasSuffix(base, ".pem"), strings.HasSuffix(base, ".key"), strings.HasSuffix(base, ".p12"), strings.HasSuffix(base, ".kdbx"):
		return true
	case strings.HasPrefix(base, "id_rsa"), strings.HasPrefix(base, "id_ed25519"):
		return true
	case strings.Contains(base, "credentials"), base == "secrets":
		return true
	}
	return false
}
