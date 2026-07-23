package workflow

// Workflow represents a parsed Forgejo Actions workflow.
type Workflow struct {
	Name string         `yaml:"name"`
	On   interface{}    `yaml:"on"`   // can be string, list, or map
	Jobs map[string]Job `yaml:"jobs"`
}

// Job is a single job within a workflow.
type Job struct {
	Name        string            `yaml:"name"`
	RunsOn      interface{}       `yaml:"runs-on"` // string or list
	Needs       []string          `yaml:"needs"`
	If          string            `yaml:"if"`
	Env         map[string]string `yaml:"env"`
	Steps       []Step            `yaml:"steps"`
	TimeoutMin  int               `yaml:"timeout-minutes"`
}

// Step is a single step within a job.
type Step struct {
	ID      string            `yaml:"id"`
	If      string            `yaml:"if"`
	Name    string            `yaml:"name"`
	Uses    string            `yaml:"uses"`
	Run     string            `yaml:"run"`
	Shell   string            `yaml:"shell"`
	With    map[string]string `yaml:"with"`
	Env     map[string]string `yaml:"env"`
	WorkingDir string         `yaml:"working-directory"`
}

// StepResult captures the outcome of executing a step.
type StepResult struct {
	Name      string
	ExitCode  int
	Status    string // success, failure, skipped
	StartTime int64  // unix timestamp
	EndTime   int64
}

// JobResult captures the outcome of executing a job.
type JobResult struct {
	Steps []StepResult
	Status string // success, failure
}
