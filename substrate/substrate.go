package substrate

import (
	"context"
	"net"
	"time"
)

// PrivateAllowance permits egress into an otherwise-denied private range (patch P4).
type PrivateAllowance struct {
	CIDR     string   // e.g. "10.1.0.11/32"
	TCPPorts []uint32 // empty = all TCP ports
}

// Egress is a sandbox's outbound policy in E2B's terms (built by U09 from spoond policies).
type Egress struct {
	AllowedCIDRs   []string
	DeniedCIDRs    []string
	AllowedDomains []string
	Private        []PrivateAllowance
}

type BuildRequest struct {
	TemplateID string // stable per image name
	BuildID    string // new UUID per build
	FromImage  string // e.g. "localhost:5000/py-base@sha256:<digest>"
	VCPU       uint32
	MemoryMB   uint32
	DiskSizeMB uint32
	StartCmd   string // may be ""
	ReadyCmd   string // may be ""
}

// BuildRefs lists every build whose data a build's artifacts reference
// (all ancestor layers plus the build itself), from E2B's SchedulingMetadata
// rootfs_build_ids and memfile_build_ids (A3 C1). U11's GC keeps them.
type BuildRefs struct {
	RootfsBuildIDs  []string
	MemfileBuildIDs []string
}

type BuildResult struct {
	BuildID            string
	KernelVersion      string
	FirecrackerVersion string
	EnvdVersion        string
	DiskSizeMB         uint32    // from metadata rootfsSizeKey
	Refs               BuildRefs // from metadata.schedulingMetadata
	Log                []string
}

type CreateRequest struct {
	TemplateID, BuildID, SandboxID                 string
	KernelVersion, FirecrackerVersion, EnvdVersion string
	VCPU, MemoryMB, DiskSizeMB                     uint32
	Resume                                         bool // true: SandboxID must be the paused sandbox's id
	EnvVars, Metadata                              map[string]string
	EndAt                                          time.Time
	Egress                                         Egress
}

type Sandbox struct {
	ID, ExecutionID, TemplateID, BuildID, HostIP string
	VCPU, MemoryMB                               uint32
	StartedAt, EndAt                             time.Time
}

type NodeInfo struct {
	Status            string // "healthy" | "draining" | "unhealthy" | "standby" | "shutting_down"
	RunningSandboxes  int
	OutstandingWork   int
	HugepagesTotal    uint64
	HugepagesUsed     uint64
	HugepagesReserved uint64
	HugepageSizeBytes uint64
}

type ExecRequest struct {
	Args    []string      // argv; spoond passes buildShellArgs(...)
	Timeout time.Duration // 0 = 30 s
	User    string        // "" = "root"
}

type ExecResult struct {
	Stdout, Stderr string
	ExitCode       int
}

type StartRequest struct {
	Args       []string
	Env        map[string]string
	Cwd        string
	PTY        bool
	Cols, Rows uint32 // PTY size; 0 = 80x24
	User       string // "" = "root"
	Stdin      bool   // true: keep the process's stdin open for Write/CloseStdin
}

type EventKind int

const (
	EventStarted EventKind = iota // PID set
	EventStdout                   // Data set
	EventStderr                   // Data set
	EventPTY                      // Data set
	EventExit                     // ExitCode set
	EventError                    // Err set
)

type ProcessEvent struct {
	Kind     EventKind
	PID      uint32
	Data     []byte
	ExitCode int
	Err      string
}

// Process is an interactive guest process. Events is closed after EventExit or EventError.
type Process interface {
	Events() <-chan ProcessEvent
	Write(data []byte) error // PTY input when started with PTY, else stdin
	Resize(cols, rows uint32) error
	Signal(kill bool) error // false = SIGTERM, true = SIGKILL
	CloseStdin() error      // non-PTY only
	Close() error           // stop streaming; does not kill the process
}

type Substrate interface {
	BuildTemplate(ctx context.Context, req BuildRequest) (BuildResult, error)
	DeleteBuild(ctx context.Context, templateID, buildID string) error

	Create(ctx context.Context, req CreateRequest) (Sandbox, error)
	List(ctx context.Context) ([]Sandbox, error)
	Delete(ctx context.Context, sandboxID string) error // nil when already gone
	Pause(ctx context.Context, sandboxID, templateID string) (buildID string, refs BuildRefs, err error)
	Checkpoint(ctx context.Context, sandboxID string) (buildID string, refs BuildRefs, err error)
	UpdateEgress(ctx context.Context, sandboxID string, eg Egress) error
	UpdateEndAt(ctx context.Context, sandboxID string, endAt time.Time) error

	NodeInfo(ctx context.Context) (NodeInfo, error)
	SetDraining(ctx context.Context, draining bool) error

	Health(ctx context.Context, sandboxID string) error // envd GET /health
	Exec(ctx context.Context, sandboxID string, req ExecRequest) (ExecResult, error)
	Start(ctx context.Context, sandboxID string, req StartRequest) (Process, error)
	DialGuest(ctx context.Context, sandboxID, hostIP string, port int) (net.Conn, error)
	TrafficToken(sandboxID string) string
}
