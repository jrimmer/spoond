package main

import (
	"runtime/debug"
	"testing"
)

func TestVersionString(t *testing.T) {
	bi := &debug.BuildInfo{GoVersion: "go1.27.1", Main: debug.Module{Version: "v2.0.0"},
		Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "0123456789abcdef0123"}, {Key: "vcs.modified", Value: "true"}}}
	cases := []struct {
		v    string
		bi   *debug.BuildInfo
		want string
	}{
		{"", bi, "spoond v2.0.0 (0123456789ab, modified) go1.27.1"},
		{"v2.0.1", bi, "spoond v2.0.1 (0123456789ab, modified) go1.27.1"},
		{"", &debug.BuildInfo{GoVersion: "go1.27.1", Main: debug.Module{Version: "(devel)"}}, "spoond dev go1.27.1"},
		{"", nil, "spoond dev"},
	}
	for _, c := range cases {
		if got := versionString(c.v, c.bi); got != c.want {
			t.Errorf("versionString(%q) = %q, want %q", c.v, got, c.want)
		}
	}
}
