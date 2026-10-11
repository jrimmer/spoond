package api

import (
	"net/http"
	"os"
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
// cached without leaking. A secret the process resolved from a
// <NAME>_FILE value file (#137 C2) is reported set with value
// "(hidden, from file)" — the env does not name it (the backend reads
// it through settings.Get), and reporting it unset would make
// `spoond config show` cry drift where the value matches.
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
	fromFile := s.fileSecretsSnapshot()
	for _, name := range settings.Known {
		v, ok := os.LookupEnv(name)
		if !ok && fromFile[name] {
			// Resolved from a <NAME>_FILE value file (C2): the env does
			// not name the secret, but the process has it — report set
			// with the value still hidden.
			out.Settings = append(out.Settings, configSetting{Name: name, Set: true, Value: configFromFileValue})
			continue
		}
		out.Settings = append(out.Settings, configSetting{
			Name:  name,
			Set:   ok,
			Value: maskSetting(name, v, ok),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// configFromFileValue is the VALUE the admin config endpoint sends for
// a secret it resolved from a value file: set, hidden, and the source
// named — config show compares set-ness, not content, and the content
// never crosses the wire (#137 C2).
const configFromFileValue = "(hidden, from file)"

// fileSecretsSnapshot copies the file-secret registry's names (a nil
// map before SetFileSecrets — no setting came from a file). The values
// were dropped at Set time; only set-ness crosses here.
func (s *Server) fileSecretsSnapshot() map[string]bool {
	s.fileSecretsMu.Lock()
	defer s.fileSecretsMu.Unlock()
	out := make(map[string]bool, len(s.fileSecrets))
	for name := range s.fileSecrets {
		out[name] = true
	}
	return out
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
// secret: settings.HideIfSecret decides (the same rule the CLI's table
// masks with).
func maskSetting(name, value string, ok bool) string {
	if !ok {
		return ""
	}
	return settings.HideIfSecret(name, value)
}
