package runner

import (
	"fmt"
	"log"

	"gopkg.in/yaml.v3"
)

// Workflow is the parsed expanded workflow payload.
type Workflow struct {
	Name string                  `yaml:"name"`
	Jobs map[string]*WorkflowJob `yaml:"jobs"`
}

// WorkflowJob is a single job in the workflow.
type WorkflowJob struct {
	RunsOn any               `yaml:"runs-on"`
	Env    map[string]string `yaml:"env"`
	Steps  []Step            `yaml:"steps"`
	// TimeoutMinutes is the job's own timeout in minutes (GitHub Actions
	// `timeout-minutes`): it bounds the whole job, the create's wait for
	// admission included. 0 means unset (the host's RUNNER_JOB_TIMEOUT,
	// or no bound).
	TimeoutMinutes TimeoutMinutes `yaml:"timeout-minutes"`
}

// TimeoutMinutes is a job's own `timeout-minutes`. GitHub accepts a
// number (integer or fractional); the field is parsed leniently so an
// expression or a string is ignored — with one log line — instead of
// failing the whole workflow parse, which is what a plain int field did
// when a workflow wrote `timeout-minutes: ${{ matrix.t }}` (M1).
// Ignoring means the job gets no bound of its own; the host's
// RUNNER_JOB_TIMEOUT still applies.
type TimeoutMinutes float64

// UnmarshalYAML accepts a YAML number. Anything else — a matrix
// expression, a quoted string, a mapping — is ignored with a log line:
// the workflow still parses and runs, just without this bound.
func (t *TimeoutMinutes) UnmarshalYAML(value *yaml.Node) error {
	switch value.Tag {
	case "!!int", "!!float":
		var f float64
		if err := value.Decode(&f); err != nil {
			log.Printf("runner: ignoring timeout-minutes %q: %v", value.Value, err)
			return nil
		}
		*t = TimeoutMinutes(f)
	case "!!null", "":
		// Unset: leave the zero value.
	default:
		log.Printf("runner: ignoring non-numeric timeout-minutes %q (an expression or string is not a job bound)", value.Value)
	}
	return nil
}

// Step is a single step in a job.
type Step struct {
	ID   string            `yaml:"id"`
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	If   string            `yaml:"if"`
	Env  map[string]string `yaml:"env"`
}

// ParseWorkflow parses the expanded workflow YAML payload.
func ParseWorkflow(payload []byte) (*Workflow, error) {
	var wf Workflow
	if err := yaml.Unmarshal(payload, &wf); err != nil {
		return nil, fmt.Errorf("parse workflow: %w", err)
	}
	return &wf, nil
}

// RunsOnLabels returns the runs-on labels for a job, handling both
// string and list forms.
func (j *WorkflowJob) RunsOnLabels() []string {
	switch v := j.RunsOn.(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	}
	return nil
}
