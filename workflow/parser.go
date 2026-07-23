package workflow

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// ParseWorkflow parses raw YAML into a Workflow struct.
func ParseWorkflow(data []byte) (*Workflow, error) {
	var wf Workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("parse workflow yaml: %w", err)
	}
	return &wf, nil
}
