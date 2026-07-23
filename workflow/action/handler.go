package action

import "context"

// Step is a reference to a workflow step (aliased to avoid import cycle).
type Step struct {
	ID         string
	Name       string
	Uses       string
	Run        string
	Shell      string
	With       map[string]string
	Env        map[string]string
	WorkingDir string
}

// Logger sends log lines back to Forgejo via UpdateLog.
type Logger interface {
	Log(line string)
	Logf(format string, args ...any)
}

// Executor runs a command in the guest VM via guest-agent.
type Executor interface {
	Exec(argv []string, env map[string]string, cwd string) (stdout, stderr string, exitCode int, err error)
	ExecStreaming(argv []string, env map[string]string, cwd string, onLine func(string)) (exitCode int, err error)
}

// Handler executes a single workflow step inside a VM.
type Handler interface {
	Execute(ctx context.Context, step *Step, exec Executor, logger Logger) (exitCode int, err error)
}
