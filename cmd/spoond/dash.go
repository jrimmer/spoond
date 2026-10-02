//go:build !nodash

package main

import (
	"github.com/jrimmer/spoond/cmd/spoond-dash"
)

func init() {
	register(command{
		name: "dash",
		desc: "read-only live dashboard of spoond's operation (basic auth)",
		run:  spoonddash.Main,
	})
}
