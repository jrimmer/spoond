//go:build !nodoctor

package main

import (
	"github.com/jrimmer/spoond/cmd/spoond-doctor"
)

func init() {
	register(command{
		name: "doctor",
		desc: "dependency/connectivity checks (orchestrator, registry, LLM, listeners, catalog)",
		run:  spoonddoctor.Main,
	})
}
