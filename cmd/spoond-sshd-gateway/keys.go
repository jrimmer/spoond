// SSH key utilities for the gateway. No build constraint — these use
// only cross-platform Go standard library + golang.org/x/crypto/ssh.

package spoondgateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

func ed25519Generate(path string) (ssh.PublicKey, ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, nil, err
	}
	block, _ := ssh.MarshalPrivateKey(priv, "")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, nil, err
	}
	os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(signer.PublicKey()), 0o644)
	return signer.PublicKey(), signer, nil
}

func loadAuthorizedKeys(spec string) ([]ssh.PublicKey, error) {
	var out []ssh.PublicKey
	if spec == "" {
		return out, nil
	}
	paths := []string{spec}
	if st, err := os.Stat(spec); err == nil && st.IsDir() {
		paths, err = filepath.Glob(filepath.Join(spec, "*.pub"))
		if err != nil {
			return nil, err
		}
	} else {
		paths = strings.Split(spec, ",")
	}
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		for len(data) > 0 {
			pk, _, _, rest, err := ssh.ParseAuthorizedKey(data)
			if err != nil {
				break
			}
			out = append(out, pk)
			data = rest
		}
	}
	return out, nil
}
