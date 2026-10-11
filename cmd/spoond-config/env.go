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
// resolveFileSecrets folds every such line onto its base name's row
// after the merge (value "(hidden, from file)", source the env file),
// so the table and the --json document talk about the settings the
// backend resolves, not the spellings.
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

// resolveFileSecrets folds every <SECRET>_FILE= assignment into its
// base name's row (#137 C2): a secret the unit hands the backend as a
// file reference is displayed as the base setting — value
// settings.HiddenFromFile (never the file's content), source the env
// file the _FILE spelling came from. A base NAME= and its FILE form
// both set is reported as the both-value error the backend will refuse
// to start with, and a lone _FILE name that is not a known secret's
// (BIND_ADDR_FILE) stays its own unknown row, the same note the
// backend's doctor gives it. The merged entries arrive in first
// appearance order; the folded rows keep the _FILE spelling's slot, so
// source and order follow the line the operator actually wrote.
func resolveFileSecrets(env []entry) []entry {
	fold := map[string]entry{}   // base name → its file-form row
	hasEnv := map[string]bool{}  // base names set by NAME=
	fileKey := map[string]bool{} // every NAME_FILE name seen
	for _, e := range env {
		if base, ok := strings.CutSuffix(e.name, "_FILE"); ok && settings.IsKnown(e.name) {
			fileKey[base] = true
			fold[base] = entry{name: base, value: settings.HiddenFromFile, source: e.source}
			continue
		}
		if e.value != "" {
			hasEnv[e.name] = true
		}
	}
	if len(fold) == 0 {
		return env
	}
	out := make([]entry, 0, len(env))
	seen := map[string]bool{}
	for _, e := range env {
		base, isFile := strings.CutSuffix(e.name, "_FILE")
		if isFile {
			if !settings.IsKnown(e.name) {
				out = append(out, e) // not a secret's file: keep the unknown row
				continue
			}
			if !seen[base] {
				seen[base] = true
				f := fold[base]
				if hasEnv[base] {
					// NAME= also set: the backend refuses to start with
					// settings.Get's both-forms error; show it as the value.
					f.value = "both set — " + base + " and " + e.name + " refuse to start"
				}
				out = append(out, f)
			}
			continue
		}
		if !fileKey[e.name] {
			out = append(out, e)
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
