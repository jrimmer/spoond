//go:build linux

package spoondgateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// withImageConfig sets the gateway's image globals (GATEWAY_SSH_IMAGES,
// GATEWAY_DEFAULT_IMAGE, GATEWAY_IMAGE_ALIASES as parsed by Main) for
// one test and restores them on cleanup.
func withImageConfig(t *testing.T, sshImgs []string, def string, aliases map[string]string) {
	t.Helper()
	oldSet, oldDef, oldAliases := sshImageSet, *defaultImage, imageAliases
	sshImageSet = map[string]bool{}
	for _, img := range sshImgs {
		sshImageSet[img] = true
	}
	*defaultImage = def
	imageAliases = map[string]string{}
	for k, v := range aliases {
		imageAliases[k] = v
	}
	t.Cleanup(func() { sshImageSet, *defaultImage, imageAliases = oldSet, oldDef, oldAliases })
}

// catalogBackend answers /api/images with the given catalog and a lease
// create with a fixed id.
func catalogBackend(images ...string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/images":
			b, _ := json.Marshal(map[string]any{"images": images})
			w.Write(b)
		case r.Method == http.MethodPost && r.URL.Path == "/api/leases":
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"0123456789abcdef0123456789abcdef"}`))
		default:
			http.NotFound(w, r)
		}
	}
}

// ctlError runs a ctl command and returns its {"error": ...} message,
// failing the test when the output is not that JSON shape.
func ctlError(t *testing.T, cmd string) string {
	t.Helper()
	out := runControlCommand(t.Context(), cmd, nil, "alice", "", "")
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil || resp.Error == "" {
		t.Fatalf("%q output = %q, want a JSON error", cmd, out)
	}
	return resp.Error
}

// createdImage returns the image of the one POST /api/leases the fake
// backend saw.
func createdImage(t *testing.T, reqs []recordedReq) string {
	t.Helper()
	var creates []recordedReq
	for _, r := range reqs {
		if r.Method == http.MethodPost && r.Path == "/api/leases" {
			creates = append(creates, r)
		}
	}
	if len(creates) != 1 {
		t.Fatalf("lease creates = %d, want 1 (requests %v)", len(creates), reqs)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(creates[0].Body), &body); err != nil {
		t.Fatalf("create body %q: %v", creates[0].Body, err)
	}
	img, _ := body["image"].(string)
	return img
}

// TestNewNoSSHImages: with GATEWAY_SSH_IMAGES empty (no default), `new`
// is refused with a message naming the setting, before any backend
// call — with or without an image named.
func TestNewNoSSHImages(t *testing.T) {
	withImageConfig(t, nil, "my-image", nil)
	reqs := withFakeBackend(t, catalogBackend("my-image"))
	for _, cmd := range []string{"new", "new my-image"} {
		if msg := ctlError(t, cmd); !strings.Contains(msg, "GATEWAY_SSH_IMAGES") {
			t.Fatalf("%q error %q does not name GATEWAY_SSH_IMAGES", cmd, msg)
		}
	}
	if len(*reqs) != 0 {
		t.Fatalf("backend saw %v, want no calls", *reqs)
	}
}

// TestNewNoDefaultImage: bare `new` on a gateway without
// GATEWAY_DEFAULT_IMAGE is refused, naming the setting and asking for
// `new <image>`; nothing is created.
func TestNewNoDefaultImage(t *testing.T) {
	withImageConfig(t, []string{"my-image"}, "", nil)
	reqs := withFakeBackend(t, catalogBackend("my-image"))
	msg := ctlError(t, "new")
	for _, want := range []string{"GATEWAY_DEFAULT_IMAGE", "new <image>"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not contain %q", msg, want)
		}
	}
	if len(*reqs) != 0 {
		t.Fatalf("backend saw %v, want no calls", *reqs)
	}
}

// TestNewDefaultImage: bare `new` creates GATEWAY_DEFAULT_IMAGE.
func TestNewDefaultImage(t *testing.T) {
	withImageConfig(t, []string{"my-image"}, "my-image", nil)
	reqs := withFakeBackend(t, catalogBackend("my-image"))
	out := runControlCommand(t.Context(), "new", nil, "alice", "", "")
	if !strings.Contains(out, `"image":"my-image"`) || !strings.Contains(out, `"created":true`) {
		t.Fatalf("new output = %q", out)
	}
	if img := createdImage(t, *reqs); img != "my-image" {
		t.Fatalf("created image %q, want my-image", img)
	}
}

// TestNewNamedImage: `new <image>` takes the name exactly as the
// backend's catalog has it. There are no built-in short names: "dev" is
// not a catalog image, so it is refused with the catalog listed, and no
// lease is created.
func TestNewNamedImage(t *testing.T) {
	withImageConfig(t, []string{"my-image"}, "", nil)
	reqs := withFakeBackend(t, catalogBackend("my-image", "ci-image"))
	out := runControlCommand(t.Context(), "new my-image", nil, "alice", "", "")
	if !strings.Contains(out, `"image":"my-image"`) {
		t.Fatalf("new my-image output = %q", out)
	}
	if img := createdImage(t, *reqs); img != "my-image" {
		t.Fatalf("created image %q, want my-image", img)
	}

	*reqs = nil
	msg := ctlError(t, "new dev")
	if !strings.Contains(msg, `unknown image "dev"`) || !strings.Contains(msg, "ci-image, my-image") {
		t.Fatalf("new dev error = %q, want the unknown image and the catalog", msg)
	}
	for _, r := range *reqs {
		if r.Method == http.MethodPost {
			t.Fatalf("new dev created a lease: %v", *reqs)
		}
	}

	// A catalog image without sshd is refused, naming GATEWAY_SSH_IMAGES.
	if msg := ctlError(t, "new ci-image"); !strings.Contains(msg, "GATEWAY_SSH_IMAGES") {
		t.Fatalf("new ci-image error = %q, want it to name GATEWAY_SSH_IMAGES", msg)
	}
}

// TestNewOperatorAlias: a GATEWAY_IMAGE_ALIASES short name still
// resolves (operator configuration, not a built-in).
func TestNewOperatorAlias(t *testing.T) {
	withImageConfig(t, []string{"my-image"}, "", map[string]string{"dev": "my-image"})
	reqs := withFakeBackend(t, catalogBackend("my-image"))
	out := runControlCommand(t.Context(), "new dev", nil, "alice", "", "")
	if !strings.Contains(out, `"image":"my-image"`) {
		t.Fatalf("new dev output = %q", out)
	}
	if img := createdImage(t, *reqs); img != "my-image" {
		t.Fatalf("created image %q, want my-image", img)
	}
}

// TestInteractiveNewNoSSHImages: an interactive `ssh new@<host>` on a
// gateway with GATEWAY_SSH_IMAGES empty gets the refusal on its session
// channel, naming the setting, and the channel closes — no hang, no
// backend call.
func TestInteractiveNewNoSSHImages(t *testing.T) {
	withImageConfig(t, nil, "", nil)
	reqs := withFakeBackend(t, catalogBackend())

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(hostKey)

	// A real loopback socket: net.Pipe is unbuffered, and both ends of
	// an SSH handshake write their version line before reading.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		if conn, err := ln.Accept(); err == nil {
			handleConn(conn, cfg, nil)
		}
	}()

	done := make(chan string, 1)
	go func() {
		clientConn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			done <- "dial: " + err.Error()
			return
		}
		c, chans, creqs, err := ssh.NewClientConn(clientConn, "gateway", &ssh.ClientConfig{
			User:            "new",
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		})
		if err != nil {
			done <- "handshake: " + err.Error()
			return
		}
		client := ssh.NewClient(c, chans, creqs)
		defer client.Close()
		ch, in, err := client.OpenChannel("session", nil)
		if err != nil {
			done <- "open: " + err.Error()
			return
		}
		go ssh.DiscardRequests(in)
		b, _ := io.ReadAll(ch)
		done <- string(b)
	}()
	select {
	case got := <-done:
		if !strings.HasPrefix(got, "spoond: ") || !strings.Contains(got, "GATEWAY_SSH_IMAGES") {
			t.Fatalf("session output = %q, want the GATEWAY_SSH_IMAGES refusal", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interactive new with no SSH images hung")
	}
	if len(*reqs) != 0 {
		t.Fatalf("backend saw %v, want no calls", *reqs)
	}
}
