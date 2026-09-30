package e2b

import (
	crand "crypto/rand"
	"math/big"

	"github.com/google/uuid"
)

const idAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// NewSandboxID returns a new sandbox id: "i" + 20 chars from [a-z0-9].
func NewSandboxID() string {
	return "i" + randomID()
}

// NewTemplateID returns a new template id: 20 chars from [a-z0-9].
func NewTemplateID() string {
	return randomID()
}

// NewUUID returns a new random UUID string.
func NewUUID() string {
	return uuid.NewString()
}

func randomID() string {
	b := make([]byte, 20)
	crand.Read(b) // crypto/rand.Read never fails
	max := big.NewInt(int64(len(idAlphabet)))
	for i := range b {
		n, _ := crand.Int(crand.Reader, max)
		b[i] = idAlphabet[n.Int64()]
	}
	return string(b)
}
