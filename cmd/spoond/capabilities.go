//go:build !nocapabilities

package main

import (
	"github.com/jrimmer/spoond/v3/cmd/spoond-capabilities"
)

func init() {
	register(command{
		name: "capabilities",
		desc: "what this spoond offers: images, sizes, policies, routes, surfaces",
		run:  spoondcapabilities.Main,
	})
}
