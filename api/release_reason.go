// Release reasons (2.5, #132 part 2): DELETE /api/leases/{id} accepts an
// optional reason that its `released` event carries, so a caller (the CI
// runner, an operator's script) can say why it let a lease go. The
// reason is free text like a lease comment, bounded and sanitised
// before it enters the event stream and the dashboard.

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxReleaseReason is the release reason's rune cap: enough for
// "ci job 3609 ✓ 11m02s" and a short sentence beside it, short enough
// that the events panel and a notifier stay readable.
const maxReleaseReason = 120

// releaseReason resolves the optional reason a DELETE carries (2.5,
// #132 part 2): the ?reason= query wins, else a JSON body {"reason"}.
// The text is sanitised (control and format characters become a plain
// space) and a reason over maxReleaseReason runes is refused. "" means
// the request carried no reason and the caller keeps its default.
func releaseReason(r *http.Request) (string, error) {
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		var body struct {
			Reason string `json:"reason"`
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			return "", fmt.Errorf("read request body: %w", err)
		}
		if len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, &body); err != nil {
				return "", fmt.Errorf("invalid JSON body")
			}
			reason = body.Reason
		}
	}
	return sanitizeReleaseReason(reason)
}

// sanitizeReleaseReason validates and cleans a release reason: invalid
// UTF-8 and more than maxReleaseReason runes are refused; every control
// or format character (which could inject terminal escapes or bidi
// overrides) becomes a space, so the text is safe to draw and log.
func sanitizeReleaseReason(reason string) (string, error) {
	if reason == "" {
		return "", nil
	}
	if !utf8.ValidString(reason) {
		return "", fmt.Errorf("reason must be valid UTF-8")
	}
	var b strings.Builder
	for _, r := range reason {
		if r != ' ' && (!unicode.IsGraphic(r) || unicode.Is(unicode.Cf, r)) {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	if utf8.RuneCountInString(b.String()) > maxReleaseReason {
		return "", fmt.Errorf("reason must be at most %d characters", maxReleaseReason)
	}
	return strings.TrimSpace(b.String()), nil
}
