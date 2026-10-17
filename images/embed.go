// Package images ships the image build context — images/ itself: the
// manifest, the dockerfiles, the guest scripts, the hooks and the
// signing key — inside the spoond binary. A release install has only
// that binary on the host, so `spoond install images` writes this
// context to /var/lib/spoond/images and builds the catalog from there,
// exactly as a build from the repo checkout would.
//
// Everything under images/ ships except README.md (operator docs) and
// this file; guest/ goes in recursively. When you add a dockerfile,
// hook or key, extend the patterns here in the same commit.
package images

import (
	"embed"
)

// Context is the docker build context the installer writes to
// /var/lib/spoond/images: manifest.yaml, every dockerfile, the
// elixir and scylla init hooks, validate-image.py, the scylla repo
// list and signing key, and guest/ (the guest init and tmux profile).
//
//go:embed manifest.yaml *.dockerfile *.sh *.py *.list *.asc *.gpg guest
var Context embed.FS
