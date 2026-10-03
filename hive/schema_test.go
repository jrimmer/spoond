// schema_test.go pins the one-table invariant behind the guide (C11):
// every field of Project has a field-table row, every known needs: key
// sits in the needs: table with its one line, and every check name has
// a description. A struct field, needs: key or check added without its
// table row would otherwise vanish from the guide, and its validation
// rule would silently stop running.
package hive

import (
	"reflect"
	"strings"
	"testing"
)

// TestFieldsCoverProject walks Project's struct fields and fails when
// the field table has no row for one.
func TestFieldsCoverProject(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range Fields() {
		if f.Name == "" {
			t.Fatal("the field table has a row with no name")
		}
		if seen[f.Name] {
			t.Errorf("the field table lists %q twice", f.Name)
		}
		seen[f.Name] = true
		// The guide renders type, default and rule verbatim; an empty
		// cell would leave the schema untaught.
		if strings.TrimSpace(f.Type) == "" || strings.TrimSpace(f.Default) == "" || strings.TrimSpace(f.Rule) == "" {
			t.Errorf("field %q: type, default and rule are all rendered in the guide; row = %+v", f.Name, f)
		}
	}
	pt := reflect.TypeOf(Project{})
	for i := 0; i < pt.NumField(); i++ {
		f := pt.Field(i)
		if f.PkgPath != "" {
			continue // unexported bookkeeping (parseErr)
		}
		key, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if key == "" {
			key = strings.ToLower(f.Name)
		}
		if !seen[key] {
			t.Errorf("Project.%s (yaml %q) has no field-table row: the guide would not teach it and Validate would not check it", f.Name, key)
		}
	}
}

// TestNeedsTableCoversKnownNeeds fails when KnownNeeds and the needs:
// table disagree, or when a key adds nothing (no line, no target).
func TestNeedsTableCoversKnownNeeds(t *testing.T) {
	inst := Instance{LeaseAPI: "lease:8890", Registry: "reg:5000"}
	var keys []string
	for _, n := range needsTable {
		keys = append(keys, n.Key)
		if strings.TrimSpace(n.Adds) == "" {
			t.Errorf("needs: key %q has no line saying what it adds to the allowlist", n.Key)
		}
		if NeedTarget(n.Key, inst) == "" {
			t.Errorf("needs: key %q maps to no allowlist target", n.Key)
		}
	}
	if !equalStrings(KnownNeeds, keys) {
		t.Errorf("KnownNeeds %v, want the needs: table's keys %v", KnownNeeds, keys)
	}
	for _, key := range KnownNeeds {
		if !isKnownNeed(key) {
			t.Errorf("isKnownNeed(%q) is false", key)
		}
	}
}

// TestCheckDescriptionsCoverChecks fails when a check name has no
// description: the guide would render a row with an empty Verifies.
func TestCheckDescriptionsCoverChecks(t *testing.T) {
	descs := CheckDescriptions()
	if len(descs) != len(CheckNames) {
		t.Fatalf("%d check descriptions, want %d (one per check, in order)", len(descs), len(CheckNames))
	}
	for i, d := range descs {
		if d.Name != CheckNames[i] {
			t.Errorf("check description %d is %q, want %q", i, d.Name, CheckNames[i])
		}
		if strings.TrimSpace(d.Verifies) == "" {
			t.Errorf("check %q has no line saying what it verifies; the guide would render an empty cell", d.Name)
		}
	}
}
