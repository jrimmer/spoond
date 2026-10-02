//go:build !nohive

package main

import (
	spoondhive "github.com/jrimmer/spoond/v2/cmd/spoond-hive"
)

func init() {
	register(command{
		name: "hive",
		desc: "swarm controller: enlistment checks for a project's hive.yaml",
		run:  spoondhive.Main,
	})
}
