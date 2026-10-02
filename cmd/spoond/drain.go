//go:build !nodrain

package main

import (
	"github.com/jrimmer/spoond/v2/cmd/spoond-drain"
)

func init() {
	register(command{
		name: "drain",
		desc: "orchestrator drain/undrain hook (pause and resume sandboxes)",
		run:  spoondrain.Main,
	})
}
