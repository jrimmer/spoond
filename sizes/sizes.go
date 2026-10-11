// Package sizes holds the lease size presets (#151): global names for a
// vCPU count and a memory size, read from SIZE_PRESETS by the backend
// (GET /api/sizes, create's `size`) and by `spoond images build` (an
// image's warm_sizes). Both processes parse the same spec, so a preset
// name means the same VM shape on both sides.
package sizes

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// DefaultSpec is SIZE_PRESETS when the variable is unset.
const DefaultSpec = "small=1:2048,medium=2:4096,large=4:8192,xlarge=8:16384"

// MinMemoryMB is the smallest memory any VM may be given, the same floor
// images/manifest.yaml sets for an image's memory_mb.
const MinMemoryMB = 128

// Preset is one named size.
type Preset struct {
	Name     string `json:"name"`
	VCPU     int    `json:"vcpu"`
	MemoryMB int    `json:"memory_mb"`
}

var nameRE = regexp.MustCompile(`^[a-z0-9-]+$`)

// Parse reads a preset spec, `name=vcpu:memory_mb` entries separated by
// commas, in order. Names are [a-z0-9-]+ and unique; vcpu is 1 or even;
// memory_mb is even and at least MinMemoryMB. Blank entries are skipped;
// a spec with no entries is an error.
func Parse(spec string) ([]Preset, error) {
	var out []Preset
	seen := map[string]bool{}
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, shape, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("size preset %q: want name=vcpu:memory_mb", entry)
		}
		name = strings.TrimSpace(name)
		if !nameRE.MatchString(name) {
			return nil, fmt.Errorf("size preset %q: name must be lower-case letters, digits and dashes", entry)
		}
		if seen[name] {
			return nil, fmt.Errorf("size preset %q: duplicate name %s", entry, name)
		}
		v, m, ok := strings.Cut(shape, ":")
		if !ok {
			return nil, fmt.Errorf("size preset %q: want name=vcpu:memory_mb", entry)
		}
		vcpu, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("size preset %q: vcpu: %w", entry, err)
		}
		mem, err := strconv.Atoi(strings.TrimSpace(m))
		if err != nil {
			return nil, fmt.Errorf("size preset %q: memory_mb: %w", entry, err)
		}
		if err := ValidVCPU(vcpu); err != nil {
			return nil, fmt.Errorf("size preset %q: %w", entry, err)
		}
		if err := ValidMemoryMB(mem); err != nil {
			return nil, fmt.Errorf("size preset %q: %w", entry, err)
		}
		seen[name] = true
		out = append(out, Preset{Name: name, VCPU: vcpu, MemoryMB: mem})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("size presets: no presets in %q", spec)
	}
	return out, nil
}

// Default returns the parsed DefaultSpec.
func Default() []Preset {
	p, err := Parse(DefaultSpec)
	if err != nil {
		panic(err) // DefaultSpec is a constant; TestParse keeps it valid
	}
	return p
}

// Lookup returns the preset named name.
func Lookup(presets []Preset, name string) (Preset, bool) {
	for _, p := range presets {
		if p.Name == name {
			return p, true
		}
	}
	return Preset{}, false
}

// DefaultImageMinMemoryMB is an image's smallest lease memory when its
// manifest sets no min_memory_mb (unless the image's own memory is
// smaller).
const DefaultImageMinMemoryMB = 512

// EffectiveMinMemoryMB is the smallest memory a lease of an image may
// ask for: minMemoryMB (the manifest's min_memory_mb) when set, else
// DefaultImageMinMemoryMB or the image's own memoryMB when that is
// smaller, so an image's default size is always allowed.
func EffectiveMinMemoryMB(minMemoryMB, memoryMB int) int {
	if minMemoryMB > 0 {
		return minMemoryMB
	}
	return min(DefaultImageMinMemoryMB, memoryMB)
}

// ValidVCPU checks the VM's vCPU rule: 1 or an even number.
func ValidVCPU(vcpu int) error {
	if vcpu < 1 || (vcpu != 1 && vcpu%2 != 0) {
		return fmt.Errorf("vcpu %d: must be 1 or an even number", vcpu)
	}
	return nil
}

// ValidMemoryMB checks the VM's memory rule: even and at least
// MinMemoryMB.
func ValidMemoryMB(mb int) error {
	if mb < MinMemoryMB || mb%2 != 0 {
		return fmt.Errorf("memory_mb %d: must be even and at least %d", mb, MinMemoryMB)
	}
	return nil
}
