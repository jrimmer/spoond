// Systemd unit environment resolution for `spoond config show` (#137).
//
// The effective environment of the backend unit is the merge of the
// unit's Environment= lines and its EnvironmentFile= files. Per
// systemd.exec(5), "variables from EnvironmentFile= override
// Environment=": systemd applies Environment= first and then, in
// listed order, each EnvironmentFile (the file's assignment wins, and
// with several files a later file wins; within one file the last
// assignment of a key wins). The same merge feeds the running-backend
// comparison and the admin call's own env, so what config show reports
// and what it authenticates with are the same values the unit starts
// the backend with.
//
// A secret may be spelled <NAME>_FILE=/path in those files (#137 C2):
// resolveFileSecrets keeps every such line as its own row (value the
// path, never the file's content) beside the base name's decorated row
// (value "(hidden, from file)"), so the table and the --json document
// talk about the settings the backend resolves, with the file reference
// visible.
package spoondconfig

import (
	"bufio"
	"strings"

	"github.com/jrimmer/spoond/v3/settings"
)

// source names where an effective value came from. Values are stable
// strings so tests can pin them; the table prints them as-is.
const (
	fromDefault = "default"
	fromEnvLine = "unit Environment="
)

// entry is one setting's effective value and where it came from. Value
// is the raw value; masking happens at print time.
type entry struct {
	name   string
	value  string
	source string
}

// parseEnvLine parses one systemd EnvironmentFile line the way
// systemd's load_env_file does: `export ` is tolerated, K=V (or bare K)
// is an assignment, quotes strip and a # only starts a comment at the
// start of a line (systemd keeps a trailing # inside the value; inline
// comments would break values that carry one).
func parseEnvLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")
	key, value, ok = strings.Cut(line, "=")
	if !ok {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') ||
			(value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
	}
	return key, value, key != ""
}

// parseEnvFile reads an EnvironmentFile's content into K=V entries in
// line order (a later line for a key wins within the file).
func parseEnvFile(content string) []entry {
	var out []entry
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		k, v, ok := parseEnvLine(sc.Text())
		if !ok {
			continue
		}
		out = append(out, entry{name: k, value: v})
	}
	return out
}

// parseEnvironmentList splits a systemd Environment= value into K=V
// entries the way systemd does: whitespace-separated assignments, with
// a quoted value allowed to carry spaces.
func parseEnvironmentList(v string) []entry {
	var out []entry
	for _, assign := range splitEnvs(v) {
		k, val, ok := parseEnvLine(assign)
		if !ok {
			continue
		}
		out = append(out, entry{name: k, value: val})
	}
	return out
}

// splitEnvs splits an Environment= value into whitespace-separated
// entries, where an entry inside double or single quotes may contain
// spaces and ends at its closing quote. The concatenation of the unit's
// Environment= lines arrives here as one string.
func splitEnvs(v string) []string {
	var out []string
	var cur strings.Builder
	sawEntry := false // an entry is being accumulated
	quote := byte(0)
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case quote != 0:
			// The closing quote itself is not part of the value.
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
		case c == '"' || c == '\'':
			quote = c
			cur.WriteByte(c)
		case c == ' ' || c == '\t' || c == '\n':
			if sawEntry {
				flush()
				sawEntry = false
			}
		default:
			sawEntry = true
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// mergeFiles is the unit env resolution order (systemd.exec(5),
// "Environment files ... override the environment variables set with
// Environment="): apply Environment= first, then each EnvironmentFile=
// in listed order, so the files' assignments override the unit lines
// and a later file overrides an earlier one.
func mergeFiles(files []string, envLine string, read func(path string) (string, error)) []entry {
	byName := map[string]entry{}
	var order []string
	apply := func(e entry, src string) {
		if _, ok := byName[e.name]; !ok {
			order = append(order, e.name)
		}
		e.source = src
		byName[e.name] = e
	}
	for _, e := range parseEnvironmentList(envLine) {
		apply(e, fromEnvLine)
	}
	for _, f := range files {
		content, err := read(f)
		if err != nil {
			continue // unreadable file: keep the rest (systemd would fail the unit on a no-prefix file)
		}
		for _, e := range parseEnvFile(content) {
			apply(e, f)
		}
	}
	out := make([]entry, 0, len(order))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out
}

// unitEnv resolves the unit's effective environment from `systemctl
// show` properties: EnvironmentFiles= (the file list) and Environment=
// (the unit's own assignments, several lines concatenated with
// spaces). read is injected so tests never touch the real filesystem
// through this path.
func unitEnv(show map[string]string, read func(path string) (string, error)) []entry {
	files := parseEnvironmentFiles(show["EnvironmentFiles"])
	return mergeFiles(files, show["Environment"], read)
}

// mergedGet builds settings.Get's get over resolved unit-env entries:
// NAME=name pairs, in apply order (the last wins, as the merge does).
func mergedGet(env []entry) func(string) string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		m[e.name] = e.value
	}
	return func(name string) string { return m[name] }
}

// refUnsetMarker is the value a <SECRET>_FILE row shows when the env
// file sets the spelling with an empty path: settings.Get resolves that
// to an unset secret, and the row says so instead of claiming a file
// backs it.
const refUnsetMarker = "(unset — no path)"

// refBothPrefix prefixes refBothMarker's stable both-set refusal text:
// a <SECRET>_FILE row whose value starts with it reports a both-set
// refusal rather than a real path, which could itself spell "both set
// —". refDecorated (below; show.go's effective and maskForDisplay
// call it) tests for the decorations through these values instead of a
// string drift between the files.
const refBothPrefix = "both set — "

// refBothMarker is the _FILE row's value when the base NAME= is spelled
// beside a non-empty path: the backend refuses to start with
// settings.Get's both-forms error, and the row names it (with no value
// in it).
func refBothMarker(base, file string) string {
	return refBothPrefix + base + " and " + file + " refuse to start"
}

// refDecorated reports whether value is one of resolveFileSecrets's
// display-only decorations for a <SECRET>_FILE row — the
// hidden-from-file constant without an env NAME=, a both-set refusal,
// or an unset marker — rather than a setting's usable value. effective
// (show.go) skips such values: the admin call's token comes from
// settings.Get over the raw merge instead (a decorated token row would
// otherwise authenticate with the decoration's text).
func refDecorated(value string) bool {
	return value == settings.HiddenFromFile || value == refUnsetMarker || strings.HasPrefix(value, refBothPrefix)
}

// resolveFileSecrets keeps every <SECRET>_FILE= assignment as its own
// row (#137 C2): the design's display surface — "config show shows the
// path for a _FILE setting (never the content)" — so the table and the
// --json document carry the ADMIN_TOKEN_FILE row: value = the path as
// the env file wrote it (a path is not the secret), source the env file
// the spelling came from. The base NAME row beside it is decorated with
// the constant settings.HiddenFromFile (never the content), so a reader
// sees both that the secret is set and which file it lives in; when no
// NAME= row stands, one is synthesized directly after the _FILE row.
// An empty path is marked refUnsetMarker (settings.Get resolves it to
// an unset secret), a base NAME= spelled beside a non-empty path turns
// the _FILE row's value into refBothMarker (the backend refuses to
// start), and a lone _FILE name that is not a known secret's
// (BIND_ADDR_FILE) stays its own unadorned row, the same note the
// backend's doctor gives it. The entries are pure display values; the
// admin call's own authentication resolves its ADMIN_TOKEN separately
// (settings.Get in run) over the raw merge, before this decoration.
func resolveFileSecrets(env []entry) []entry {
	fileKey := map[string]bool{}  // base names with a <BASE>_FILE line
	filePath := map[string]bool{} // base names whose _FILE line names a path
	envSet := map[string]bool{}   // names set directly by a non-empty NAME=
	for _, e := range env {
		base, ok := strings.CutSuffix(e.name, "_FILE")
		if !ok || !settings.IsKnown(e.name) {
			if e.value != "" {
				envSet[e.name] = true
			}
			continue
		}
		fileKey[base] = true
		if e.value != "" {
			filePath[base] = true
		}
	}
	if len(fileKey) == 0 {
		return env
	}
	out := make([]entry, 0, len(env)+len(fileKey))
	synthed := map[string]bool{} // base rows synthesized beside a _FILE row
	for _, e := range env {
		base, isFile := strings.CutSuffix(e.name, "_FILE")
		switch {
		case !isFile:
			if fileKey[e.name] && filePath[e.name] && !envSet[e.name] {
				// The secret resolves from the file (the env NAME= is unset
				// or empty): the base row shows the hidden-from-file
				// constant, never the env value. With NAME= also non-empty
				// the resolution refuses (the _FILE row says so), and the
				// base row keeps its own env value, masked at print time —
				// "from file" would lie about a secret Get refuses to
				// resolve.
				out = append(out, entry{name: e.name, value: settings.HiddenFromFile, source: e.source})
				continue
			}
			out = append(out, e)
		case !settings.IsKnown(e.name):
			out = append(out, e) // not a secret's file: keep the unknown row
		default:
			f := e
			switch {
			case e.value == "":
				f.value = refUnsetMarker
			case envSet[base] && filePath[base]:
				f.value = refBothMarker(base, e.name)
			}
			out = append(out, f)
			if f.value != refUnsetMarker && !synthed[base] && !envSet[base] {
				// No NAME= row stands for the base: add one next to the
				// _FILE row, so the setting's set-ness reads at a glance.
				synthed[base] = true
				out = append(out, entry{name: base, value: settings.HiddenFromFile, source: f.source})
			}
		}
	}
	return out
}

// parseEnvironmentFiles reads the EnvironmentFiles= property: a
// whitespace-separated list of `path (ignore_errors=yes|no)` entries.
// Each entry ends at its "(ignore_errors=…)" annotation — split on
// that, not on whitespace (a path may contain spaces, quoted). The
// backend unit lists a single file.
func parseEnvironmentFiles(s string) []string {
	var out []string
	var cur strings.Builder
	inParen := false
	flush := func() {
		item := strings.TrimSpace(cur.String())
		cur.Reset()
		if item == "" {
			return
		}
		if len(item) > 1 && item[0] == '"' && item[len(item)-1] == '"' {
			item = item[1 : len(item)-1]
		}
		out = append(out, item)
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '(':
			inParen = true
		case c == ')':
			inParen = false
			flush()
		default:
			if !inParen {
				cur.WriteByte(c)
			}
		}
	}
	flush() // a trailing path without the annotation
	return out
}
