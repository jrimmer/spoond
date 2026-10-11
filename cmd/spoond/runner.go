//go:build !norunner

package main

import (
	"github.com/jrimmer/spoond/v3/cmd/spoond-runner"
)

func init() {
	register(command{
		name: "runner",
		desc: "Forgejo Actions runner (adaptive pool)",
		run:  spoondrunner.Main,
	})
}
