package settings

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// envRead matches the ways spoond code reads a setting by name.
var envRead = regexp.MustCompile(`\b(os\.Getenv|os\.LookupEnv|envIntOr|envOr|envDuration|envDurationOr|envDurationOrZero|envBoolOr|pct|limit|get|envInt|envBool|getenv|mustEnv|envFloat|env|envPct)\("([A-Z][A-Z0-9_]{2,})"`)

// envTable matches a setting named in a {"NAME", &dst} table, the way
// the conformance harness lists its required variables.
var envTable = regexp.MustCompile(`\{"([A-Z][A-Z0-9_]{2,})", &`)

// TestKnownMatchesSource: Known is exactly the settings the source reads
// — a new setting must be added to it, and a removed one dropped.
func TestKnownMatchesSource(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			switch rel {
			case ".claude", ".git", "tests", "node_modules":
				return filepath.SkipDir
			}
			if strings.HasSuffix(rel, "/gen") || strings.Contains(rel, "/gen/") {
				return filepath.SkipDir
			}
			return nil
		}
		// The conformance suite is test-only: its settings are in its
		// _test.go files. Everywhere else a _test.go file only sets
		// environment for a test.
		test := strings.HasSuffix(p, "_test.go")
		if !strings.HasSuffix(p, ".go") || (test && !strings.HasPrefix(rel, "conformance/")) {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range envRead.FindAllSubmatch(b, -1) {
			found[string(m[2])] = true
		}
		for _, m := range envTable.FindAllSubmatch(b, -1) {
			found[string(m[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var missing, stale []string
	for k := range found {
		if !IsKnown(k) {
			missing = append(missing, k)
		}
	}
	for _, k := range Known {
		if !found[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 || len(stale) > 0 {
		t.Fatalf("settings.Known is out of date: add %v, remove %v", missing, stale)
	}
	if !slices.IsSorted(Known) {
		t.Fatal("settings.Known must stay sorted")
	}
}

// TestCheckEnvFile: unknown and repeated keys are reported, comments,
// blank lines and export prefixes are understood, and values never
// appear in the result.
func TestCheckEnvFile(t *testing.T) {
	unknown, dup := CheckEnvFile(`# backend
ADMIN_TOKEN=s3cret
export POOL_SIZE=2
BURST_RESERVE_MIB=4096

POOL_SIZE=3
SIZE_PRESET=small=1:2048
`)
	if !slices.Equal(unknown, []string{"BURST_RESERVE_MIB", "SIZE_PRESET"}) {
		t.Fatalf("unknown = %v", unknown)
	}
	if !slices.Equal(dup, []string{"POOL_SIZE"}) {
		t.Fatalf("duplicate = %v", dup)
	}
}
