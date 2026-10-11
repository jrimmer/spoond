package sizes

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		name, spec string
		want       []Preset
		err        string // substring; "" = no error
	}{
		{name: "default", spec: DefaultSpec, want: []Preset{
			{"small", 1, 2048}, {"medium", 2, 4096}, {"large", 4, 8192}, {"xlarge", 8, 16384},
		}},
		{name: "spaces and blank entries", spec: " tiny = 1 : 512 ,, big-2=2:1024 ", want: []Preset{
			{"tiny", 1, 512}, {"big-2", 2, 1024},
		}},
		{name: "memory floor", spec: "a=1:128", want: []Preset{{"a", 1, 128}}},
		{name: "empty", spec: "", err: "no presets"},
		{name: "no equals", spec: "small", err: "want name=vcpu:memory_mb"},
		{name: "no colon", spec: "small=1", err: "want name=vcpu:memory_mb"},
		{name: "upper-case name", spec: "Small=1:2048", err: "name must be"},
		{name: "empty name", spec: "=1:2048", err: "name must be"},
		{name: "duplicate", spec: "a=1:2048,a=2:4096", err: "duplicate name a"},
		{name: "odd vcpu", spec: "a=3:2048", err: "vcpu 3"},
		{name: "zero vcpu", spec: "a=0:2048", err: "vcpu 0"},
		{name: "vcpu not a number", spec: "a=x:2048", err: "vcpu"},
		{name: "odd memory", spec: "a=1:2049", err: "memory_mb 2049"},
		{name: "memory below floor", spec: "a=1:126", err: "memory_mb 126"},
		{name: "memory not a number", spec: "a=1:2g", err: "memory_mb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.spec)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("Parse(%q) err = %v, want one containing %q", tc.spec, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.spec, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Parse(%q) = %+v, want %+v", tc.spec, got, tc.want)
			}
		})
	}
}

func TestEffectiveMinMemoryMB(t *testing.T) {
	for _, tc := range []struct{ minMB, memory, want int }{
		{1024, 2048, 1024}, // set: the manifest wins
		{0, 2048, 512},     // unset: 512
		{0, 256, 256},      // unset, small image: its own memory
	} {
		if got := EffectiveMinMemoryMB(tc.minMB, tc.memory); got != tc.want {
			t.Errorf("EffectiveMinMemoryMB(%d, %d) = %d, want %d", tc.minMB, tc.memory, got, tc.want)
		}
	}
}

func TestLookup(t *testing.T) {
	presets := Default()
	if p, ok := Lookup(presets, "large"); !ok || p != (Preset{"large", 4, 8192}) {
		t.Fatalf("Lookup(large) = %+v, %v", p, ok)
	}
	if _, ok := Lookup(presets, "huge"); ok {
		t.Fatal("Lookup(huge) found a preset")
	}
}
