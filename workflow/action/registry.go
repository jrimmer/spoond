package action

import (
	"strings"
)

var handlers = map[string]Handler{
	"actions/checkout":   &CheckoutHandler{},
	"actions/setup-go":   &SetupGoHandler{},
	"actions/setup-node": &SetupNodeHandler{},
}

// Resolve maps a "uses" string (e.g. "actions/checkout@v4") to a Handler.
func Resolve(uses string) (Handler, bool) {
	key := strings.Split(uses, "@")[0]
	h, ok := handlers[key]
	return h, ok
}
