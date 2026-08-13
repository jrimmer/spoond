//go:build linux

package api

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

// dialInNetns enters the named network namespace and dials addr.
// Used to connect to the forkd-agent (port 8888) inside a sandbox VM
// from the host. The netns must exist under /var/run/netns/.
func dialInNetns(netns, addr string) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		f, err := os.Open(filepath.Join("/var/run/netns", netns))
		if err != nil {
			ch <- result{nil, fmt.Errorf("open netns %s: %w", netns, err)}
			return
		}
		defer f.Close()
		if err := unix.Setns(int(f.Fd()), unix.CLONE_NEWNET); err != nil {
			ch <- result{nil, fmt.Errorf("setns %s: %w", netns, err)}
			return
		}
		d, err := net.DialTimeout("tcp", addr, 10*time.Second)
		ch <- result{d, err}
	}()
	r := <-ch
	return r.conn, r.err
}
