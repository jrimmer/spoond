//go:build !noconfig

package main

import (
	"github.com/jrimmer/spoond/v4/cmd/spoond-config"
)

func init() {
	register(command{
		name: "config",
		desc: "show unit env settings: value, source, running-backend drift",
		run:  spoondconfig.Main,
	})
}
