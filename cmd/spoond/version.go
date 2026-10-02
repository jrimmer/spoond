package main

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// version can be set at build time (-ldflags "-X main.version=v2.0.0").
// Otherwise the module version Go embeds is used: the release tag when
// built from a tagged checkout or `go install ...@vX`, a pseudo-version
// for other commits.
var version = ""

func init() {
	register(command{name: "version", desc: "print the version, commit and Go version", run: func([]string) int {
		fmt.Println(versionString(version, readBuildInfo()))
		return 0
	}})
}

var readBuildInfo = func() *debug.BuildInfo {
	bi, _ := debug.ReadBuildInfo()
	return bi
}

// versionString renders "spoond <version> (<commit>[, modified]) <go version>".
func versionString(v string, bi *debug.BuildInfo) string {
	rev, modified, gov := "", false, ""
	if bi != nil {
		gov = bi.GoVersion
		if v == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
	}
	if v == "" {
		v = "dev"
	}
	var meta []string
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev != "" {
		meta = append(meta, rev)
	}
	if modified {
		meta = append(meta, "modified")
	}
	out := "spoond " + v
	if len(meta) > 0 {
		out += " (" + strings.Join(meta, ", ") + ")"
	}
	if gov != "" {
		out += " " + gov
	}
	return out
}
