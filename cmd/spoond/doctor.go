//go:build !nodoctor

package main

import (
	"github.com/jrimmer/spoond/cmd/spoond-doctor"
)

func init() {
	register(command{
		name: "doctor",
		desc: "dependency/connectivity checks (forkd, LLM, listeners, pool)",
		run:  spoonddoctor.Main,
	})
}
