package spoondconfig

import (
	"os/exec"
	"strings"
)

// systemctlProps is the command runner: `systemctl show <unit> -p
// <prop>...`. Tests replace it, so no test runs a real systemctl.
var systemctlProps = func(unit string, props ...string) (map[string]string, error) {
	args := []string{"show", unit}
	for _, p := range props {
		args = append(args, "-p", p)
	}
	b, err := exec.Command("systemctl", args...).Output()
	if err != nil {
		return nil, err
	}
	return parseShow(string(b)), nil
}

// parseShow reads `systemctl show -p …` output into property -> value.
// A property can come on several lines — a unit with two
// EnvironmentFile= lines prints EnvironmentFiles= twice — so repeated
// lines join with a space, in order, instead of the last one winning.
func parseShow(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		if prev, ok := m[k]; ok && prev != "" {
			v = prev + " " + v
		}
		m[k] = v
	}
	return m
}
