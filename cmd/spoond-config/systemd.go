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
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		out[k] = v
	}
	return out, nil
}
