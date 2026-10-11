//go:build !nogateway && linux

package main

import (
	"github.com/jrimmer/spoond/v3/cmd/spoond-sshd-gateway"
)

func init() {
	register(command{
		name: "gateway",
		desc: "SSH gateway + ctl plane",
		run:  spoondgateway.Main,
	})
}
