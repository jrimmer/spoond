// Package images ships spoond's guest pieces — images/guest: the
// template start command (spoond-guest-init) and the tmux profile for
// interactive SSH logins (spoond-tmux.sh) — inside the spoond binary.
// spoond ships no images: an operator's Dockerfile copies these files
// in (README.md says how), and `spoond install images` writes them to
// /var/lib/spoond/guest so a build context can take them from there.
//
// README.md (operator docs) and this file stay out. When you add a
// guest file, it ships through the guest pattern below.
package images

import (
	"embed"
)

// Context is guest/ as the installer writes it under /var/lib/spoond:
// guest/spoond-guest-init and guest/spoond-tmux.sh.
//
//go:embed guest
var Context embed.FS
