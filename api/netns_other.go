//go:build !linux

package api

import (
	"fmt"
	"net"
)

// dialInNetns is a stub on non-Linux platforms. The real implementation
// (netns_linux.go) uses unix.Setns/CLONE_NEWNET which are Linux-only.
// This stub allows the api package to compile on macOS for testing;
// it is never called in production (spoond runs on Linux).
func dialInNetns(netns, addr string) (net.Conn, error) {
	return nil, fmt.Errorf("dialInNetns: not supported on this platform (netns=%s addr=%s)", netns, addr)
}
