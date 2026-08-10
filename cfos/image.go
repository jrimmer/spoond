package cfos

import (
	"fmt"
	"strings"
)

// imageFor maps a CFOS executeCode language/capability to a forkd image
// tag (ticket #17 U2). Defaults to the adapter's default image for
// unknown/empty languages; explicit unsupported declarations are
// rejected with a clear error.
func imageFor(language, def string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "", "javascript", "js", "typescript", "ts", "workers":
		return def, nil
	case "go", "golang":
		return "go-base", nil
	case "python", "py":
		return "py-base", nil
	case "elixir", "exs":
		return "elixir-base", nil
	default:
		return "", fmt.Errorf("no image for language %q", language)
	}
}

// ImageFor is the exported form used by the adapter.
var ImageFor = imageFor
