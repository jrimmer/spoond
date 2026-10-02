//go:build !noimages

package main

import (
	spoondimages "github.com/jrimmer/spoond/v2/cmd/spoond-images"
)

func init() {
	register(command{
		name: "images",
		desc: "image pipeline: registry push and E2B template builds",
		run:  spoondimages.Main,
	})
}
