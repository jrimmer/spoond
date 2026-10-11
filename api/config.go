package api

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jrimmer/spoond/v3/settings"
)

// GET /api/admin/config (#137): what each setting is in this backend
// process, so `spoond config show` (cmd/spoond-config) can compare the
// unit's env files against the running backend. Auth is adminOK like
// the other /api/admin routes: ADMIN_TOKEN or an admin user's token.
//
// The answer lists every name in settings.Known with os.LookupEnv in
// this process and whether it is set. Secrets are masked at the
// endpoint too: a TOKEN, KEY, SECRET, PASSWORD, HASH or SEED name never
// carries its value over the wire, so the response can be logged or
// cached without leaking.
//
// The response:
//
//	{"started_at": <RFC3339 process start>,
//	 "settings": [{"name","set":bool,"value"}]}
//
// started_at is when the server was built (Server.startedAt), so a
// caller can tell whether the backend restarted since the unit env
// changed. settings.Known is already sorted, so the list is stable.
func (s *Server) handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(w, r) {
		return
	}
	out := configResponse{StartedAt: s.startedAt.Format(time.RFC3339), Settings: []configSetting{}}
	for _, name := range settings.Known {
		v, ok := os.LookupEnv(name)
		out.Settings = append(out.Settings, configSetting{
			Name:  name,
			Set:   ok,
			Value: maskSetting(name, v, ok),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// configResponse is the GET /api/admin/config body.
type configResponse struct {
	StartedAt string          `json:"started_at"`
	Settings  []configSetting `json:"settings"`
}

// configSetting is one setting of that body. An unset one is
// set:false, value "".
type configSetting struct {
	Name  string `json:"name"`
	Set   bool   `json:"set"`
	Value string `json:"value"`
}

// maskSetting masks a setting's value when the name says it is a
// secret. When ok is false the value is empty regardless.
func maskSetting(name, value string, ok bool) string {
	if !ok {
		return ""
	}
	return HideIfSecret(name, value)
}

// HideIfSecret is the masking rule (#137): a setting whose name
// carries TOKEN, KEY, SECRET, PASSWORD, HASH or SEED is never shown —
// "(hidden)" stands in for its value. Every surface that prints setting
// values (this endpoint and `spoond config show`) masks through it.
func HideIfSecret(name, value string) string {
	upper := strings.ToUpper(name)
	for _, marker := range []string{"TOKEN", "KEY", "SECRET", "PASSWORD", "HASH", "SEED"} {
		if strings.Contains(upper, marker) {
			return "(hidden)"
		}
	}
	return value
}
