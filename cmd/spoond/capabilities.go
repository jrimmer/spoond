//go:build !nocapabilities

package main

import (
	"github.com/jrimmer/spoond/v3/api"
	"github.com/jrimmer/spoond/v3/cmd/spoond-capabilities"
)

func init() {
	// The API's capability document reports the same version string
	// `spoond version` prints (cmd/spoond/version.go), set here so the
	// endpoint's `version` field is the linked binary's.
	api.SetVersion(func() string { return versionString(version, readBuildInfo()) })
	register(command{
		name: "capabilities",
		desc: "what this spoond offers: images, sizes, policies, routes, surfaces",
		run:  spoondcapabilities.Main,
	})
}
