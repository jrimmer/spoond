package e2b

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// EnvdToken mints the envd access token for a sandbox:
// hex(HMAC-SHA256(seed, id)).
func (c *Client) EnvdToken(id string) string {
	return hmacToken(c.cfg.TokenSeed, id)
}

// TrafficToken mints the traffic access token for a sandbox:
// hex(HMAC-SHA256(seed, "sandbox-traffic-"+id)).
func (c *Client) TrafficToken(id string) string {
	return hmacToken(c.cfg.TokenSeed, "sandbox-traffic-"+id)
}

func hmacToken(seed []byte, id string) string {
	m := hmac.New(sha256.New, seed)
	m.Write([]byte(id))
	return hex.EncodeToString(m.Sum(nil))
}
