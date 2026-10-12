// Lease comments (#132): the free-text annotation a create body may
// carry and the comment route sets later. One validator serves both
// routes (handleCreate and handleComment) and the service's
// setComment, so the two surfaces can never drift apart — a comment a
// create accepts is exactly one the comment route accepts too.
package api

import "fmt"

// maxComment is the comment's cap in bytes: long enough for the CI
// runner's "forgejo job <id> <url>" label plus a sentence beside it,
// short enough that the dashboard line and the created event cut at
// 120 stay readable.
const maxComment = 512

// validateComment checks a lease comment: at most maxComment bytes and
// no control characters — anything below 0x20 (a newline included) and
// 0x7F, which could smuggle terminal escapes into the dashboard and a
// notifier's log lines. The release reason is sanitised instead of
// refused; a comment is refused whole, so nothing silently rewrites a
// caller's text. The empty comment is valid: through the comment route
// it clears the annotation. Non-ASCII printable text ("é ✓") passes
// untouched.
func validateComment(comment string) error {
	if len(comment) > maxComment {
		return fmt.Errorf("comment must be <= 512 chars")
	}
	for _, r := range comment {
		if r < 0x20 || r == 0x7F {
			return fmt.Errorf("comment must not contain control characters")
		}
	}
	return nil
}
