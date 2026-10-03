// Package env resolves configuration variables that were renamed in 2.0.
//
// spoond 2.0 renamed its FORKD_-prefixed configuration variables to
// SPOOND_ (docs/setup.md, "Renamed in 2.0"). Configuration reads go
// through Get, which prefers the SPOOND_ name and falls back to the
// deprecated FORKD_ name, logging a one-line deprecation warning the
// first time a deprecated name supplies a value in this process.
package env

import (
	"log"
	"os"
	"sync"
)

// deprecated maps each deprecated variable name to its 2.0 name.
var deprecated = map[string]string{
	"FORKD_BACKEND_URL":  "SPOOND_BACKEND_URL",
	"FORKD_AGENT_TOKEN":  "SPOOND_AGENT_TOKEN",
	"FORKD_IMAGE":        "SPOOND_IMAGE",
	"FORKD_LLM_MODEL":    "SPOOND_LLM_MODEL",
	"FORKD_CTL_HOST":     "SPOOND_CTL_HOST",
	"FORKD_CTL_PORT":     "SPOOND_CTL_PORT",
	"FORKD_CTL_KEY":      "SPOOND_CTL_KEY",
	"FORKD_GATEWAY_HOST": "SPOOND_GATEWAY_HOST",
	"FORKD_NO_TMUX":      "SPOOND_NO_TMUX",
}

// renamed is the inverse of deprecated: 2.0 name → deprecated name.
var renamed = func() map[string]string {
	m := make(map[string]string, len(deprecated))
	for old, name := range deprecated {
		m[name] = old
	}
	return m
}()

var (
	mu     sync.Mutex
	warned = map[string]bool{}
)

// Get returns the value of the environment variable name, or def when
// it is unset or empty. For a renamed variable, Get also consults the
// deprecated pre-2.0 name: the fallback works, and logs a one-line
// deprecation warning once per name per process. When both names are
// set, the 2.0 name wins.
func Get(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	if old, ok := renamed[name]; ok {
		if v := os.Getenv(old); v != "" {
			warnOnce(old, name)
			return v
		}
	}
	return def
}

func warnOnce(old, name string) {
	mu.Lock()
	defer mu.Unlock()
	if warned[old] {
		return
	}
	warned[old] = true
	log.Printf("%s is deprecated: use %s (the old name still works in 2.0)", old, name)
}
