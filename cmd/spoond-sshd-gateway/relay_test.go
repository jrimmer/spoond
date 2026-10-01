//go:build linux

package spoondgateway

import (
	"reflect"
	"testing"
)

// TestCommandForRequest pins the command selection: shell, exec, sftp,
// the PTY rule (pty iff pty-req, never for sftp) and the refusal of any
// other subsystem.
func TestCommandForRequest(t *testing.T) {
	t.Run("shell", func(t *testing.T) {
		cmd, ok := commandForRequest("shell", "", false)
		if !ok {
			t.Fatal("shell refused")
		}
		want := []string{"/bin/bash", "-l"}
		if !reflect.DeepEqual(cmd.Args, want) {
			t.Fatalf("args = %v, want %v", cmd.Args, want)
		}
		if cmd.Pty {
			t.Fatal("shell without pty-req must not use a PTY")
		}
		if cmd.Kind != "shell" {
			t.Fatalf("kind = %q", cmd.Kind)
		}
	})

	t.Run("shell with pty", func(t *testing.T) {
		cmd, ok := commandForRequest("shell", "", true)
		if !ok || !cmd.Pty {
			t.Fatalf("ok=%v pty=%v, want a PTY for shell with pty-req", ok, cmd.Pty)
		}
	})

	t.Run("exec", func(t *testing.T) {
		cmd, ok := commandForRequest("exec", "ls -la /tmp", true)
		if !ok {
			t.Fatal("exec refused")
		}
		want := []string{"/bin/bash", "-c", "ls -la /tmp"}
		if !reflect.DeepEqual(cmd.Args, want) {
			t.Fatalf("args = %v, want %v", cmd.Args, want)
		}
		if !cmd.Pty {
			t.Fatal("exec with pty-req must use a PTY")
		}
	})

	t.Run("exec without pty", func(t *testing.T) {
		cmd, ok := commandForRequest("exec", "true", false)
		if !ok || cmd.Pty {
			t.Fatalf("ok=%v pty=%v, want no PTY", ok, cmd.Pty)
		}
	})

	t.Run("sftp", func(t *testing.T) {
		cmd, ok := commandForRequest("subsystem", "sftp", true)
		if !ok {
			t.Fatal("sftp refused")
		}
		want := []string{"/bin/sh", "-c",
			"if [ -x /usr/lib/openssh/sftp-server ]; then exec /usr/lib/openssh/sftp-server; else exec /usr/lib/ssh/sftp-server; fi"}
		if !reflect.DeepEqual(cmd.Args, want) {
			t.Fatalf("args = %v, want %v", cmd.Args, want)
		}
		if cmd.Pty {
			t.Fatal("sftp must never use a PTY, even with pty-req")
		}
	})

	t.Run("unknown subsystem is refused", func(t *testing.T) {
		for _, sub := range []string{"netconf", "sftp-server", ""} {
			if cmd, ok := commandForRequest("subsystem", sub, false); ok {
				t.Fatalf("subsystem %q accepted: %+v", sub, cmd)
			}
		}
	})

	t.Run("other request types are refused", func(t *testing.T) {
		if _, ok := commandForRequest("pty-req", "", true); ok {
			t.Fatal("pty-req is not a command")
		}
	})
}

// TestSessionEnv pins the env composition: collected env passes through,
// TERM defaults to xterm-256color, SSH_CONNECTION/SSH_CLIENT carry the
// client address and the host service address, and the root account
// fields are set.
func TestSessionEnv(t *testing.T) {
	got := sessionEnv(map[string]string{"LANG": "C.UTF-8", "FOO": "bar"},
		"vt100", "192.168.1.7", "5522")
	want := map[string]string{
		"LANG":           "C.UTF-8",
		"FOO":            "bar",
		"TERM":           "vt100",
		"SSH_CONNECTION": "192.168.1.7 5522 10.1.0.11 22",
		"SSH_CLIENT":     "192.168.1.7 5522 22",
		"USER":           "root",
		"HOME":           "/root",
		"LOGNAME":        "root",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env = %v, want %v", got, want)
	}

	// No pty-req: TERM falls back to xterm-256color.
	got = sessionEnv(nil, "", "10.0.0.1", "22")
	if got["TERM"] != "xterm-256color" {
		t.Fatalf("default TERM = %q", got["TERM"])
	}
	if len(got) != 6 {
		t.Fatalf("env without collected pairs = %v", got)
	}
}
