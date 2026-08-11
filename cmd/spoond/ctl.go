//go:build !noctl

package main

import (
	"github.com/jrimmer/spoond/cmd/spoondctl"
)

func init() {
	register(command{
		name: "ctl",
		desc: "control-plane CLI (thin ssh ctl@ wrapper)",
		run:  spoondctl.Main,
	})
}
