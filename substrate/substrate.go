package substrate

import (
	"context"
	"net"
	"os"
	"strconv"
	"time"
)

// DefaultBuildTimeout is how long a template build may run before the
// image pipeline gives up on it and the GC treats the row as stale. The
// two share it (spoond-4yl): the pipeline bounds BuildTemplate with it,
// and the GC fails any build still in state building for more than twice
// it, so a SIGKILL or reboot mid-build can never leave a permanent GC
// root. Override it with SPOOND_BUILD_TIMEOUT (a Go duration or a number
// of seconds); the image pipeline and the backend's ServiceConfig
// BuildTimeout both read that variable, so the two sides stay on one
// knob.
const DefaultBuildTimeout = 60 * time.Minute

// BuildTimeoutFromEnv resolves the template build timeout from
// SPOOND_BUILD_TIMEOUT, falling back to DefaultBuildTimeout when unset,
// malformed or not positive. A plain integer is read as seconds.
func BuildTimeoutFromEnv() time.Duration {
	v := os.Getenv("SPOOND_BUILD_TIMEOUT")
	if v == "" {
		return DefaultBuildTimeout
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n > 0 {
			return time.Duration(n) * time.Second
		}
		return DefaultBuildTimeout
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return DefaultBuildTimeout
}

// PrivateAllowance permits egress into an otherwise-denied private range (patch P4).
type PrivateAllowance struct {
	CIDR     string   // e.g. "10.0.0.11/32"
	TCPPorts []uint32 // empty = all TCP ports
}

// Egress is a sandbox's outbound policy in E2B's terms (built by U09 from spoond policies).
type Egress struct {
	AllowedCIDRs   []string
	DeniedCIDRs    []string
	AllowedDomains []string
	Private        []PrivateAllowance
	// GuestDNS reports that the deployment configures one or more private
	// guest resolvers (SPOOND_GUEST_DNS_ADDR, comma-separated) and grants
	// them port 53, so domains resolve without the public DNS fallback and
	// none is added. False means the guest relies on a public resolver and
	// the fallback is needed.
	GuestDNS bool
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
	Version           string // orchestrator build version ("" when unknown)
	RunningSandboxes  int
	OutstandingWork   int
	HugepagesTotal    uint64
	HugepagesUsed     uint64
	HugepagesReserved uint64
	HugepageSizeBytes uint64
	// EnvdVersion and FirecrackerVersion are the host's envd and
	// firecracker versions when the substrate can report them ("" when
	// unknown). They gate a named-snapshot start: a version whose saved
	// envd/firecracker differs from the host cannot run here (2.7, #83
	// S2).
	EnvdVersion        string
	FirecrackerVersion string
}

// FreeHugepageBytes is the hugepage memory neither used nor reserved,
// in bytes. It saturates at 0: a reading taken while sandboxes start or
// stop can briefly report used+reserved above total, and the unsigned
// subtraction would otherwise wrap to an enormous free figure that
// admits anything.
func (n NodeInfo) FreeHugepageBytes() uint64 {
	taken := n.HugepagesUsed + n.HugepagesReserved
	if taken >= n.HugepagesTotal {
		return 0
	}
	return (n.HugepagesTotal - taken) * n.HugepageSizeBytes
}

type ExecRequest struct {
	Args    []string          // argv; spoond passes buildShellArgs(...)
	Env     map[string]string // process environment, never argv
	Timeout time.Duration     // 0 = 30 s
	User    string            // "" = "root"
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

// FileInfo is a path's metadata as returned by Substrate.Stat.
type FileInfo struct {
	Name    string
	Size    int64
	Mode    os.FileMode
	ModTime time.Time
	IsDir   bool
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

	// WriteFile creates path (with parents) and writes data, replacing an
	// existing file. ErrNotFound when the sandbox is unknown.
	WriteFile(ctx context.Context, sandboxID, path string, data []byte, mode os.FileMode) error
	// ReadFile reads a file, error when it is larger than max bytes.
	ReadFile(ctx context.Context, sandboxID, path string, max int64) ([]byte, error)
	// Stat returns a path's metadata. ErrNotFound when it does not exist.
	Stat(ctx context.Context, sandboxID, path string) (FileInfo, error)
	// MakeDir creates path and any missing parents. ErrNotFound when the
	// sandbox is unknown.
	MakeDir(ctx context.Context, sandboxID, path string, mode os.FileMode) error
	// Rename moves oldPath to newPath, replacing newPath. It backs the
	// atomic guest-file writes (2.7, #83): write a temp file, rename it
	// over the target. ErrNotFound when the source does not exist.
	Rename(ctx context.Context, sandboxID, oldPath, newPath string) error
	// Remove deletes path; recursive removes non-empty directories.
	Remove(ctx context.Context, sandboxID, path string, recursive bool) error
}
