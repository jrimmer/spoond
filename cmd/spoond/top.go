//go:build !nodash

package main

import (
	"github.com/jrimmer/spoond/v2/cmd/spoond-dash"
)

func init() {
	register(command{
		name: "top",
		desc: "the dashboard grid in the terminal (same sources as dash)",
		run:  spoonddash.Top,
	})
}
