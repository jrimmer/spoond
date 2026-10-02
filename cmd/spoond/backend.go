//go:build !nobackend

package main

import (
	"github.com/jrimmer/spoond/v2/cmd/spoond-backend"
)

func init() {
	register(command{
		name: "backend",
		desc: "lease API (warm pool, proxy, LLM gateway)",
		run:  spoondbackend.Main,
	})
}
