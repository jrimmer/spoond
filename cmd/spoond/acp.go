//go:build !noacp

package main

import (
	"github.com/jrimmer/spoond/cmd/spoond-acp"
)

func init() {
	register(command{
		name: "acp",
		desc: "Agent Client Protocol endpoint",
		run:  spoondacp.Main,
	})
}
