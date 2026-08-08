package runner

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Workflow is the parsed expanded workflow payload.
type Workflow struct {
	Name string                  `yaml:"name"`
	Jobs map[string]*WorkflowJob `yaml:"jobs"`
}

// WorkflowJob is a single job in the workflow.
type WorkflowJob struct {
	RunsOn any    `yaml:"runs-on"`
	Steps  []Step `yaml:"steps"`
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
