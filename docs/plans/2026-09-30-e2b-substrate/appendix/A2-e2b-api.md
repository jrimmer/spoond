# E2B infra: orchestrator, template-manager and envd API facts (verbatim)

Source: `github.com/e2b-dev/infra` at commit `e473dd130015ca1ff8cf9300341e25034c38e178` (blobless clone, HEAD `e473dd13`).
All paths are relative to the repo root. Every quote below is copied byte-for-byte from the
file at that commit; the header above each block gives the path and line range. Prose outside
code blocks is commentary and is marked as such where it infers rather than quotes.

## 0. Quick reference (commentary, each point backed by a quote below)

| Thing | Value | Where |
|---|---|---|
| Orchestrator gRPC port | `5008` (`GRPC_PORT`); SandboxService, VolumeService, ChunkService, TemplateService (when the node runs the template-manager role), InfoService and grpc.health all on the one listener (cmux shares it with HTTP) | §1.5 |
| gRPC transport security | plaintext (`insecure.NewCredentials()`) | §1.5 |
| Orchestrator sandbox proxy port | `5007` (`PROXY_PORT`) | §5 |
| envd port inside the guest | `49983` | §4, §5 |
| envd RPC style | Connect (connectrpc.com/connect) over HTTP/1.1, paths `/process.Process/*`, `/filesystem.Filesystem/*` | §2 |
| Routing to a sandbox through the proxy | Host `<port>-<sandboxID>-<anything>.<domain>`, or, when Host is `localhost`/an IP/`sandbox.<domain>`, headers `E2b-Sandbox-Id` + `E2b-Sandbox-Port` | §5 |
| envd auth header | `X-Access-Token: <envd_access_token>` (only if the sandbox was created with one) | §4 |
| envd user selection | `Authorization: Basic base64("<user>:")` | §4 |
| Non-envd port auth when ingress token set | `e2b-traffic-access-token` header | §5 |
| envd_access_token | hex(HMAC-SHA256(seed, sandboxID)) | §3.1 |
| traffic_access_token | hex(HMAC-SHA256(seed, "sandbox-traffic-" + sandboxID)) | §3.1 |
| sandbox_id | `"i"` + 20 chars from `[a-z0-9]`; validator regex `^[a-z0-9]+$` | §8 |
| template_id | 20 chars from `[a-z0-9]` (`id.Generate()`) | §8 |
| build_id | UUID v4 (`uuid.NewRandom()`; DB default `gen_random_uuid()`); orchestrator `uuid.Parse`s it on pause | §8 |
| execution_id | `uuid.New().String()` per start/resume | §8 |
| team_id | UUID string | §8 |
| kernel_version example | `vmlinux-6.1.158` | §6.6 |
| firecracker_version examples | `v1.14-0.2.0` (current default), `v1.12.1_210cbac`, `v1.10.1_30cbb07` | §6.6 |
| envd_version example | `0.9.0` (output of `envd -version`, no leading v) | §6.6 |
| huge_pages | `true` iff firecracker release line >= 1.7 | §3.2 |


---

## 1. Orchestrator protos (complete) and generated Go code

### 1.1 `packages/orchestrator/orchestrator.proto` (complete)

Imports only `google/protobuf/empty.proto` and `google/protobuf/timestamp.proto` (well-known types; nothing else from the repo).

`packages/orchestrator/orchestrator.proto` lines 1-265 (of 265)

```proto
syntax = "proto3";

import "google/protobuf/empty.proto";
import "google/protobuf/timestamp.proto";

option go_package = "https://github.com/e2b-dev/infra/orchestrator";

message SandboxConfig {
  // Data required for creating a new sandbox.
  string template_id = 1;
  string build_id = 2;

  string kernel_version = 3;
  string firecracker_version = 4;

  bool huge_pages = 5;

  string sandbox_id = 6;
  map<string, string> env_vars = 7;

  // Metadata about the sandbox.
  map<string, string> metadata = 8;
  optional string alias = 9;
  string envd_version = 10;

  int64 vcpu = 11;
  int64 ram_mb = 12;

  string team_id = 13;
  // Maximum length of the sandbox in Hours.
  int64 max_sandbox_length = 14;

  int64 total_disk_size_mb = 15;

  bool snapshot = 16;
  string base_template_id = 17;

  bool auto_pause = 18;

  optional string envd_access_token = 19;
  string execution_id = 20;

  // Whether the sandbox should have access to the internet.
  // This is optional only for backwards compatibility.
  // After migration, the optional keyword can be removed.
  optional bool allow_internet_access = 21;

  optional SandboxNetworkConfig network = 22;

  repeated SandboxVolumeMount volumeMounts = 23;

  // Auto-resume policy for paused sandboxes.
  optional SandboxAutoResumeConfig auto_resume = 24;

  // When true, a timeout auto-pause takes a filesystem-only snapshot (no
  // memory) instead of a full memory snapshot. Only meaningful when auto_pause
  // is set. Passed through so the policy survives an API re-sync from the
  // orchestrator's sandbox list (mirrors auto_pause); the orchestrator itself
  // does not act on it — the API evictor does.
  bool auto_pause_filesystem_only = 25;

  // Retention of sandbox events in days
  int64 events_ttl_days = 26;

  // Sandbox workload identity configuration requested at create time. Absent on
  // older serialized configs, which decode as no workload identity.
  optional SandboxIam iam = 27;
}

// Sandbox workload identity configuration. A non-empty tokens map defines the
// sandbox workload identity, which the orchestrator derives from the existing
// trusted team_id, sandbox_id, execution_id and template_id fields. No
// credential is minted, signed, or delivered here.
message SandboxIam {
  // Named workload-token definitions, keyed by a caller-chosen token name.
  map<string, SandboxIamToken> tokens = 1;
}

// A named workload-token definition. file_path is intentionally omitted: only
// absent/null is accepted at admission, so file delivery is not represented.
message SandboxIamToken {
  string audience = 1;
  string token_type = 2;
}

message SandboxAutoResumeConfig {
  // Policy values are owned by the API layer today (e.g. "off", "any").
  string policy = 1;
  // Timeout requested on initial sandbox create (seconds).
  uint64 timeout_seconds = 2;
}

message SandboxVolumeMount {
  string id = 1;
  string path = 2;
  string type = 3;
  string name = 4;
}

message SandboxNetworkConfig {
  optional SandboxNetworkEgressConfig egress = 1;
  optional SandboxNetworkIngressConfig ingress = 2;
}

message SandboxNetworkTransform {
  map<string, string> headers = 1;
}

message SandboxNetworkRule {
  optional SandboxNetworkTransform transform = 1;
}

message SandboxNetworkDomainRules {
  repeated SandboxNetworkRule rules = 1;
}

message SandboxNetworkEgressConfig {
  repeated string allowed_cidrs = 1;
  repeated string denied_cidrs = 2;
  repeated string allowed_domains = 3;
  map<string, SandboxNetworkDomainRules> rules = 4;

  // BYOP SOCKS5 egress proxy.
  string egress_proxy_address = 5;
  string egress_proxy_username = 6;
  string egress_proxy_password = 7;
}

message SandboxNetworkIngressConfig {
  optional string traffic_access_token = 1;
  optional string mask_request_host = 2;
  repeated uint32 https_ports = 3;
}

message SandboxCreateRequest {
  SandboxConfig sandbox = 1;

  google.protobuf.Timestamp start_time = 2;
  google.protobuf.Timestamp end_time = 3;

  // When true, resume by cold-booting from the snapshot's rootfs even when it
  // includes a memory snapshot. The request can only widen toward the no-memory
  // path — it can never force a memory restore of a snapshot that has none.
  // Absent = the snapshot's own metadata alone selects the boot path, so
  // existing callers are unaffected. Request-scoped: nothing durable records it.
  optional bool filesystem_boot = 4;
}

message SandboxCreateResponse {
  string client_id = 1;
  SchedulingMetadata scheduling_metadata = 2;

  // Echoes whether the sandbox cold-booted from its rootfs (vs a memory
  // restore). Absent from orchestrators that predate filesystem_boot, so a
  // caller that demanded a filesystem boot can detect an unhonored demand
  // instead of trusting deploy ordering.
  bool filesystem_boot_applied = 3;

  // The Firecracker version the sandbox actually RUNS: the request's
  // declared version resolved through the firecracker-versions flag at
  // start, frozen for the sandbox's lifetime. Callers gating features on the
  // FC version must read this rather than re-deriving it — a re-resolution
  // can disagree with the frozen value whenever the flag changes or the
  // evaluation contexts differ. Empty from orchestrators that predate the
  // field.
  string resolved_firecracker_version = 4;
}

message SandboxUpdateRequest {
  string sandbox_id = 1;

  // All fields are optional — only set fields are applied.
  optional google.protobuf.Timestamp end_time = 2;
  optional SandboxNetworkEgressConfig egress = 3;
}

message SandboxDeleteRequest {
  string sandbox_id = 1;
  optional string kill_reason = 2;
}

message SandboxPauseRequest {
  string sandbox_id = 1;
  string template_id = 2;
  string build_id = 3;

  // When true, persist only the filesystem (no memory snapshot); resuming such
  // a snapshot cold-boots (reboots) from the rootfs. Default false = full memory
  // snapshot, so existing callers are unaffected.
  bool filesystem_only = 4;
}

message SchedulingMetadata {
  // memfile_base_build_id / rootfs_base_build_id are each artifact's root layer
  // (shared across the template's sandboxes); they can differ. build_id is the
  // final/current layer. All also appear in the lists below.
  string memfile_base_build_id = 1;
  string build_id = 2;
  // Deduplicated build IDs whose data each artifact references (all ancestor
  // layers plus the build itself). Sorted; order is not significant. When a
  // list exceeds the cap, the lightest layers are dropped first and the count
  // of dropped layers is reported.
  repeated string memfile_build_ids = 3;
  repeated string rootfs_build_ids = 4;
  uint32 memfile_dropped_builds = 5;
  uint32 rootfs_dropped_builds = 6;
  // Referenced bytes per build, aligned with the *_build_ids lists. On save the
  // new memfile layer's bytes are a pre-dedup, block-granular upper bound.
  repeated uint64 memfile_build_bytes = 7;
  repeated uint64 rootfs_build_bytes = 8;
  string rootfs_base_build_id = 9;
}

message SandboxPauseResponse {
  SchedulingMetadata scheduling_metadata = 1;
}

message SandboxCheckpointRequest {
  string sandbox_id = 1;
  string build_id = 3;
  // Provenance stamped onto the snapshot's storage objects for the storage
  // index (e.g. template_id). Opaque to the orchestrator, which just forwards
  // it to object metadata.
  map<string, string> metadata = 4;
}

message SandboxCheckpointResponse {
  SchedulingMetadata scheduling_metadata = 1;
}

message RunningSandbox {
  // Deprecated: the API no longer rebuilds sandbox state from this list. Redis
  // is the source of truth; List is only used to detect sandboxes running on a
  // node that the store does not know about, so they can be killed. The fields
  // below carry everything that decision needs. Still populated so API
  // instances predating those fields keep working during a rollout.
  SandboxConfig config = 1 [deprecated = true];
  string client_id = 2;

  google.protobuf.Timestamp start_time = 3;
  google.protobuf.Timestamp end_time = 4;

  // Minimal set required to detect an orphaned sandbox and kill it.
  // sandbox_id + team_id form the store key. execution_id is carried in the
  // edge sandbox-catalog delete event, so a cluster node's routing entry is
  // evicted too. vcpu/ram_mb feed the node's optimistic resource accounting.
  string sandbox_id = 5;
  string team_id = 6;
  string execution_id = 7;
  int64 vcpu = 8;
  int64 ram_mb = 9;
}

message SandboxListResponse {
  repeated RunningSandbox sandboxes = 1;
}

service SandboxService {
  rpc Create(SandboxCreateRequest) returns (SandboxCreateResponse);
  rpc Update(SandboxUpdateRequest) returns (google.protobuf.Empty);
  rpc List(google.protobuf.Empty) returns (SandboxListResponse);
  rpc Delete(SandboxDeleteRequest) returns (google.protobuf.Empty);
  rpc Pause(SandboxPauseRequest) returns (SandboxPauseResponse);
  rpc Checkpoint(SandboxCheckpointRequest) returns (SandboxCheckpointResponse);
}
```

### 1.2 `packages/orchestrator/template-manager.proto` (complete)

Imports `orchestrator.proto` (for `SchedulingMetadata`, quoted in 1.1 above) plus the two well-known types.

`packages/orchestrator/template-manager.proto` lines 1-189 (of 189)

```proto
syntax = "proto3";

import "google/protobuf/empty.proto";
import "google/protobuf/timestamp.proto";
import "orchestrator.proto";

option go_package = "https://github.com/e2b-dev/infra/template-manager";


message InitLayerFileUploadRequest {
  string templateID = 1;
  string hash = 2;
  optional string cacheScope = 3;
}

message InitLayerFileUploadResponse{
  bool present = 1;
  optional string url = 2;
  // Request headers the upload client must send on the PUT to `url`.
  map<string, string> headers = 3;
}

message TemplateStep {
  string type = 1;
  repeated string args = 2;
  optional bool force = 3;

  optional string filesHash = 4;
}

message FromTemplateConfig {
  string alias = 1;

  string buildID = 2;
}

// AWS ECR registry authentication
message AWSRegistry {
  string awsAccessKeyId = 1;
  string awsSecretAccessKey = 2;
  string awsRegion = 3;
}

// GCP registry authentication
message GCPRegistry {
  string serviceAccountJson = 1;
}

// General registry authentication with username/password
message GeneralRegistry {
  string username = 1;
  string password = 2;
}

// Docker registry authentication configuration
message FromImageRegistry {
  oneof type {
    AWSRegistry aws = 1;
    GCPRegistry gcp = 2;
    GeneralRegistry general = 3;
  }
}

message TemplateConfig {
  string templateID = 1;
  string buildID = 2;

  int32 memoryMB = 3;
  int32 vCpuCount = 4;
  int32 diskSizeMB = 5;

  // Deprecated: template-manager now selects the kernel and firecracker versions itself
  string kernelVersion = 6 [deprecated = true];
  string firecrackerVersion = 7 [deprecated = true];
  string startCommand = 8;
  // Deprecated: hugePages is derived from the resolved firecracker version locally
  bool hugePages = 9 [deprecated = true];

  string readyCommand = 10;

  optional bool force = 12;
  repeated TemplateStep steps = 13;

  oneof source {
     string fromImage = 11;
     FromTemplateConfig fromTemplate = 14;
   }

  optional FromImageRegistry fromImageRegistry = 15;

  string teamID = 16;

  // Optional during the rolling upgrade. When absent, the template manager
  // uses diskSizeMB as the post-build free-space target.
  optional int32 freeDiskSizeMB = 17;
}

message TemplateCreateRequest {
  TemplateConfig template = 1;
  optional string cacheScope = 2;
  optional string version = 3;
}

enum LogLevel {
    Debug = 0;
    Info = 1;
    Warn = 2;
    Error = 3;
}

enum LogsDirection {
  Forward = 0;
  Backward = 1;
}

message TemplateStatusRequest {
  string templateID = 1;
  string buildID = 2;
  optional int32 offset = 3;
  optional LogLevel level = 4;
  optional uint32 limit = 5;

  optional google.protobuf.Timestamp start = 6;
  optional google.protobuf.Timestamp end = 7;
  optional LogsDirection direction = 8;
}

// Data required for deleting a template.
message TemplateBuildDeleteRequest {
  string buildID = 1;
  string templateID = 2;
}

message TemplateBuildMetadata {
  int32 rootfsSizeKey = 1;
  string envdVersionKey = 2;

  // Versions actually used by the template-manager to build the template.
  // The API persists these into the env_builds row once the build finishes.
  string kernelVersion = 3;
  string firecrackerVersion = 4;
  // Shared with the orchestrator so the API can match the build's scheduling
  // metadata against create/resume responses without a separate format.
  SchedulingMetadata schedulingMetadata = 5;
}

enum TemplateBuildState {
  Building = 0;
  Failed = 1;
  Completed = 2;
}

message TemplateBuildLogEntry {
  google.protobuf.Timestamp timestamp = 1;
  string message = 2;
  LogLevel level = 3;
  map<string, string> fields = 4;
}

message TemplateBuildStatusReason {
  string message = 1;
  optional string step = 2;
}

// Logs from template build
message TemplateBuildStatusResponse {
  reserved 3, 4;

  TemplateBuildState status = 1;
  TemplateBuildMetadata metadata = 2;
  repeated TemplateBuildLogEntry logEntries = 5;

  optional TemplateBuildStatusReason reason = 6;
}

// Interface exported by the server.
service TemplateService {
  // TemplateCreate is a gRPC service that creates a new template
  rpc TemplateCreate (TemplateCreateRequest) returns (google.protobuf.Empty);

  // TemplateStatus is a gRPC service that streams the status of a template build
  rpc TemplateBuildStatus (TemplateStatusRequest) returns (TemplateBuildStatusResponse);

  // TemplateBuildDelete is a gRPC service that deletes files associated with a template build
  rpc TemplateBuildDelete (TemplateBuildDeleteRequest) returns (google.protobuf.Empty);

  // InitLayerFileUpload requests an upload URL for a tar file containing layer files to be cached for the template build.
  rpc InitLayerFileUpload (InitLayerFileUploadRequest) returns (InitLayerFileUploadResponse);
}
```

### 1.3 `packages/orchestrator/info.proto` (complete)

`packages/orchestrator/info.proto` lines 1-95 (of 95)

```proto
syntax = "proto3";

import "google/protobuf/empty.proto";
import "google/protobuf/timestamp.proto";

option go_package = "https://github.com/e2b-dev/infra/orchestrator";

// needs to be different from the enumeration in the template manager
enum ServiceInfoStatus {
  Healthy = 0;
  // Draining excludes new work while existing work finishes; it can return to Healthy.
  Draining = 1;
  Unhealthy = 2;
  // Standby means the node is not actively used, but it can return to Healthy and continues serving traffic.
  Standby = 3;
  // ShuttingDown drains existing work before process exit and cannot be reversed.
  ShuttingDown = 4;
}

enum ServiceInfoRole {
  TemplateBuilder = 0;
  Orchestrator = 1;
}

message DiskMetrics {
  string mount_point = 1;
  string device = 2;
  string filesystem_type = 3;
  uint64 used_bytes = 4;
  uint64 total_bytes = 5;
}

message MachineInfo {
  string cpu_architecture = 1;
  string cpu_family = 2;
  string cpu_model = 3;
  string cpu_model_name = 4;
  repeated string cpu_flags = 5;
}

message ServiceInfoResponse {
  string node_id = 1;
  string service_id = 2;
  string service_version = 3;
  string service_commit = 4;

  ServiceInfoStatus service_status = 51;
  repeated ServiceInfoRole service_roles = 52;
  google.protobuf.Timestamp service_startup = 53;
  MachineInfo machine_info = 54;
  repeated string labels = 55;
  google.protobuf.Timestamp service_status_changed_at = 56;

  // Overlapping work holds, not distinct sandboxes or builds. Zero is idle.
  uint64 outstanding_work = 58;

  // Nonpositive values reject sandbox creation.
  int64 max_sandboxes = 59;

  int64 metric_vcpu_used = 101 [deprecated = true];
  int64 metric_memory_used_mb = 102 [deprecated = true];
  int64 metric_disk_mb = 103 [deprecated = true];
  uint32 metric_sandboxes_running = 104;

  // Host system usage metrics
  uint32 metric_cpu_percent = 105;
  uint64 metric_memory_used_bytes = 106;

  // Host system total resources
  uint32 metric_cpu_count = 108;
  uint64 metric_memory_total_bytes = 109;

  // Allocated resources to sandboxes
  uint32 metric_cpu_allocated = 110;
  uint64 metric_memory_allocated_bytes = 111;
  uint64 metric_disk_allocated_bytes = 112;

  // Detailed disk metrics for each mount point
  repeated DiskMetrics metric_disks = 113;

  // Hugepage pool metrics (page counts from /proc/meminfo)
  uint64 metric_hugepages_total = 114;
  uint64 metric_hugepages_used = 115;
  uint64 metric_hugepages_reserved = 116;
  uint64 metric_hugepage_size_bytes = 117;
}

message ServiceStatusChangeRequest {
  ServiceInfoStatus service_status = 2;
}

service InfoService {
  rpc ServiceInfo(google.protobuf.Empty) returns (ServiceInfoResponse);
  rpc ServiceStatusOverride(ServiceStatusChangeRequest) returns (google.protobuf.Empty);
}
```

### 1.4 `go_package` options and where the generated Go code lives

The `go_package` options in the .proto files are URL-shaped placeholders
(`https://github.com/e2b-dev/infra/orchestrator`, `https://github.com/e2b-dev/infra/template-manager`);
they are NOT the import paths. The real output paths are fixed by the `protoc` flags in `generate.go`
(`paths=source_relative` into `packages/shared/pkg/grpc/...`) and by `M` mapping for the cross-file import:

`packages/orchestrator/generate.go` lines 1-8 (of 8)

```go
package main

//go:generate mise exec -- protoc --go_out=../shared/pkg/grpc/orchestrator/ --go_opt=paths=source_relative --go-grpc_out=../shared/pkg/grpc/orchestrator/ --go-grpc_opt=paths=source_relative orchestrator.proto
//go:generate mise exec -- protoc --go_out=../shared/pkg/grpc/orchestrator/ --go_opt=paths=source_relative --go-grpc_out=../shared/pkg/grpc/orchestrator/ --go-grpc_opt=paths=source_relative volume.proto
//go:generate mise exec -- protoc --go_out=../shared/pkg/grpc/orchestrator/ --go_opt=paths=source_relative --go-grpc_out=../shared/pkg/grpc/orchestrator/ --go-grpc_opt=paths=source_relative chunks.proto
//go:generate mise exec -- protoc --go_out=../shared/pkg/grpc/orchestrator-info/ --go_opt=paths=source_relative --go-grpc_out=../shared/pkg/grpc/orchestrator-info/ --go-grpc_opt=paths=source_relative info.proto
//go:generate mise exec -- protoc --go_out=../shared/pkg/grpc/template-manager/ --go_opt=paths=source_relative --go_opt=Morchestrator.proto=github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator --go-grpc_out=../shared/pkg/grpc/template-manager/ --go-grpc_opt=paths=source_relative template-manager.proto
//go:generate mise exec -- env GOOS=linux mockery
```

Resulting Go packages (all in module `github.com/e2b-dev/infra/packages/shared`, `go 1.26.8`,
`google.golang.org/grpc v1.83.2`, `google.golang.org/protobuf v1.36.12`, `connectrpc.com/connect v1.18.1`;
generated with `protoc-gen-go v1.36.11`, `protoc v7.34.1`):

| Proto | Go import path | Go package name | Files |
|---|---|---|---|
| orchestrator.proto (+ volume.proto, chunks.proto) | `github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator` | `orchestrator` | `orchestrator.pb.go`, `orchestrator_grpc.pb.go` (also `volume*.pb.go`, `chunks*.pb.go`) |
| info.proto | `github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator-info` | `orchestrator` (sic; the API imports it as `orchestratorinfo`/`infogrpc`) | `info.pb.go`, `info_grpc.pb.go` |
| template-manager.proto | `github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager` | `template_manager` | `template-manager.pb.go`, `template-manager_grpc.pb.go` |
| envd process.proto | `github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process` (+ `/processconnect`) | `process`, `processconnect` | `process.pb.go`, `processconnect/process.connect.go` |
| envd filesystem.proto | `github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/filesystem` (+ `/filesystemconnect`) | `filesystem`, `filesystemconnect` | `filesystem.pb.go`, `filesystemconnect/filesystem.connect.go` |

(Commentary) A Go client can `go get github.com/e2b-dev/infra/packages/shared@<commit>` and import these
directly; the package directories are not under `internal/`. The shared module drags a large dependency
set, so vendoring/regenerating the five .proto files is the lighter alternative; the generate commands
above plus the buf config in §2.4 reproduce the same packages.

Header of the generated orchestrator file (confirms package name and generator versions):

`packages/shared/pkg/grpc/orchestrator/orchestrator.pb.go` lines 1-8 (of 1929)

```go
// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.36.11
// 	protoc        v7.34.1
// source: orchestrator.proto

package orchestrator

```

`volume.proto` and `chunks.proto` define `VolumeService` and `ChunkService` (persistent volumes and
peer chunk reads). They are not imported by the three protos above; only their service lines are quoted here:

`packages/orchestrator/volume.proto` lines 176-193 (of 193)

```proto
service VolumeService {
  // volume operations
  rpc CreateVolume(CreateVolumeRequest) returns (CreateVolumeResponse);
  rpc DeleteVolume(DeleteVolumeRequest) returns (DeleteVolumeResponse);

  // directory operations
  rpc CreateDir(CreateDirRequest) returns (CreateDirResponse);
  rpc ListDir(ListDirRequest) returns (ListDirResponse);

  // file operations
  rpc CreateFile(stream CreateFileRequest) returns (CreateFileResponse);
  rpc GetFile(GetFileRequest) returns (stream GetFileResponse);

  // path operations (file, dir, other?)
  rpc DeletePath(DeletePathRequest) returns (DeletePathResponse);
  rpc StatPath(StatPathRequest) returns (StatPathResponse);
  rpc UpdatePath(UpdatePathRequest) returns (UpdatePathResponse);
}
```

`packages/orchestrator/chunks.proto` lines 67-76 (of 76)

```proto
service ChunkService {
  // GetBuildFileSize returns the total size of a seekable diff file (memfile, rootfs.ext4).
  rpc GetBuildFileSize(GetBuildFileSizeRequest) returns (GetBuildFileSizeResponse);
  // GetBuildFileExists checks if a blob file is present in the peer's local cache.
  rpc GetBuildFileExists(GetBuildFileExistsRequest) returns (GetBuildFileExistsResponse);
  // ReadAtBuildSeekable streams a range from a seekable diff file (memfile, rootfs.ext4).
  rpc ReadAtBuildSeekable(ReadAtBuildSeekableRequest) returns (stream ReadAtBuildSeekableResponse);
  // GetBuildBlob streams an entire blob file (snapfile, metadata, headers).
  rpc GetBuildBlob(GetBuildBlobRequest) returns (stream GetBuildBlobResponse);
}
```

### 1.5 How the API dials the orchestrator, and what the orchestrator serves on the port

One plaintext gRPC connection per node, all four service clients on it:

`packages/api/internal/orchestrator/nodemanager/client.go` lines 1-49 (of 49)

```go
package nodemanager

import (
	"fmt"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/clusters"
	orchestratorinfo "github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator-info"
)

var OrchestratorToApiNodeStateMapper = map[orchestratorinfo.ServiceInfoStatus]api.NodeStatus{
	orchestratorinfo.ServiceInfoStatus_Healthy:      api.NodeStatusReady,
	orchestratorinfo.ServiceInfoStatus_Draining:     api.NodeStatusDraining,
	orchestratorinfo.ServiceInfoStatus_Unhealthy:    api.NodeStatusUnhealthy,
	orchestratorinfo.ServiceInfoStatus_Standby:      api.NodeStatusStandby,
	orchestratorinfo.ServiceInfoStatus_ShuttingDown: api.NodeStatusShuttingDown,
}

func NewClient(tracerProvider trace.TracerProvider, meterProvider metric.MeterProvider, host string) (*clusters.GRPCClient, error) {
	conn, err := grpc.NewClient(host,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(
			otelgrpc.NewClientHandler(
				otelgrpc.WithTracerProvider(tracerProvider),
				otelgrpc.WithMeterProvider(meterProvider),
			),
		),
		grpc.WithKeepaliveParams(
			keepalive.ClientParameters{
				Time:                30 * time.Second, // Send ping every 30s
				Timeout:             5 * time.Second,  // Wait 5s for response
				PermitWithoutStream: true,             // Allow pings even without active streams
			},
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to establish GRPC connection: %w", err)
	}

	return clusters.NewGRPCClient(conn, "orchestrator"), nil
}
```

`packages/api/internal/clusters/client.go` lines 15-35 (of 48)

```go
type GRPCClient struct {
	Info     infogrpc.InfoServiceClient
	Sandbox  orchestratorgrpc.SandboxServiceClient
	Volumes  orchestratorgrpc.VolumeServiceClient
	Template templatemanagergrpc.TemplateServiceClient

	Connection *grpc.ClientConn

	observeTarget string
}

func NewGRPCClient(conn *grpc.ClientConn, observeTarget string) *GRPCClient {
	return &GRPCClient{
		Connection:    conn,
		Info:          infogrpc.NewInfoServiceClient(conn),
		Sandbox:       orchestratorgrpc.NewSandboxServiceClient(conn),
		Volumes:       orchestratorgrpc.NewVolumeServiceClient(conn),
		Template:      templatemanagergrpc.NewTemplateServiceClient(conn),
		observeTarget: observeTarget,
	}
}
```

`packages/shared/pkg/consts/sandboxes.go` lines 1-17 (of 17)

```go
package consts

import (
	"strconv"

	"github.com/e2b-dev/infra/packages/shared/pkg/env"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

const NodeIDLength = 8

// ClientID Sandbox ID client part used during migration when we are still returning client but its no longer serving its purpose,
// and we are returning it only for backward compatibility with SDK clients.
// We don't want to use some obviously dummy value such as empty zeros, because for users it will look like something is wrong with the sandbox id
const ClientID = "6532622b"

var OrchestratorAPIPort = uint16(utils.Must(strconv.ParseUint(env.GetEnv("ORCHESTRATOR_PORT", "5008"), 10, 16)))
```

Server side (orchestrator process): all services registered on one `grpcServer`; TemplateService only
when the node runs the template-manager role; cmux shares the TCP port between gRPC and HTTP/1:

`packages/orchestrator/pkg/factories/run.go` lines 983-1040 (of 1248)

```go

	grpcServer := e2bgrpc.NewGRPCServer(tel, e2bgrpc.WithSandboxResumeMetrics())
	orchestrator.RegisterSandboxServiceServer(grpcServer, orchestratorService)
	orchestrator.RegisterVolumeServiceServer(grpcServer, volumeService)
	orchestrator.RegisterChunkServiceServer(grpcServer, orchestratorService)

	// template manager
	var tmpl *tmplserver.ServerStore
	var localUploadHandler *localupload.Handler
	if services.RunsTemplateManager() {
		buildPersistence, uploadHandler, err := setupBuildStorage(ctx, limiter, config)
		if err != nil {
			logger.L().Fatal(ctx, "failed to setup build storage", zap.Error(err))
		}

		localUploadHandler = uploadHandler

		tmpl, err = tmplserver.New(
			ctx,
			config,
			serviceInfo,
			featureFlags,
			tel.MeterProvider,
			globalLogger,
			tmplSbxLoggerExternal,
			sandboxFactory,
			sandboxProxy,
			templateCache,
			persistence,
			buildPersistence,
			uploads,
		)
		if err != nil {
			logger.L().Fatal(ctx, "failed to create template manager", zap.Error(err))
		}

		templatemanager.RegisterTemplateServiceServer(grpcServer, tmpl)

		closers = append(closers, closer{"template server", tmpl.Close})
	}

	infoService := service.NewInfoService(serviceInfo, sandboxes, hostMetrics)
	orchestratorinfo.RegisterInfoServiceServer(grpcServer, infoService)

	grpcHealth := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, grpcHealth)

	// cmux server, allows us to reuse the same TCP port between grpc and HTTP requests
	cmuxServer, err := NewCMUXServer(ctx, config.GRPCPort, tel.MeterProvider)
	if err != nil {
		logger.L().Fatal(ctx, "failed to create cmux server", zap.Error(err))
	}

	// Create all matchers BEFORE starting Serve() to avoid data race.
	// cmux.Match() modifies internal state that Serve() reads from.
	httpListener := cmuxServer.Match(cmux.HTTP1Fast())
	grpcListener := cmuxServer.Match(cmux.Any()) // the rest are GRPC requests

```

`packages/orchestrator/pkg/cfg/model.go` lines 85-85 (of 207)

```go
	GRPCPort                    uint16            `env:"GRPC_PORT"                     envDefault:"5008"`
```

`packages/orchestrator/pkg/cfg/model.go` lines 97-97 (of 207)

```go
	ProxyPort                   uint16            `env:"PROXY_PORT"                    envDefault:"5007"`
```

gRPC metadata the API adds to Create (only `is_resume` matters for a local node; the catalog event
metadata is only for remote "cluster" nodes behind the edge client-proxy):

`packages/api/internal/orchestrator/nodemanager/metadata.go` lines 1-86 (of 86)

```go
package nodemanager

import (
	"context"
	"strconv"

	"google.golang.org/grpc/metadata"

	"github.com/e2b-dev/infra/packages/api/internal/clusters"
	"github.com/e2b-dev/infra/packages/shared/pkg/edge"
	grpcshared "github.com/e2b-dev/infra/packages/shared/pkg/grpc"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

type NodeMetadata struct {
	// Service instance ID is unique identifier for every orchestrator process, after restart it will change.
	// In the future, we want to migrate to using this ID instead of node ID for tracking orchestrators-
	ServiceInstanceID string

	Commit  string
	Version string
}

func (n *Node) setMetadata(md NodeMetadata) {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	n.meta = md
}

func (n *Node) Metadata() NodeMetadata {
	n.mutex.RLock()
	defer n.mutex.RUnlock()

	return n.meta
}

func (n *Node) GetSandboxCreateCtx(ctx context.Context, req *orchestrator.SandboxCreateRequest) (*clusters.GRPCClient, context.Context) {
	md := metadata.MD{}

	if n.IsClusterNode() {
		md = edge.SerializeSandboxCatalogCreateEvent(
			edge.SandboxCatalogCreateEvent{
				SandboxID:               req.GetSandbox().GetSandboxId(),
				SandboxMaxLengthInHours: req.GetSandbox().GetMaxSandboxLength(),
				SandboxStartTime:        req.GetStartTime().AsTime(),

				ExecutionID:    req.GetSandbox().GetExecutionId(),
				OrchestratorID: n.Metadata().ServiceInstanceID,
			},
		)
	}

	// Pass snapshot (is_resume) via metadata so the server-side stats handler
	// can include it in otelgrpc metric attributes during TagRPC.
	md.Set(grpcshared.IsResumeMetadataKey, strconv.FormatBool(req.GetSandbox().GetSnapshot()))

	// Merge medata from client (auth, routing with service instance id) and event metadata.
	return n.client, appendMetadataCtx(ctx, md)
}

func (n *Node) GetSandboxDeleteCtx(ctx context.Context, sandboxID string, executionID string, restoreOnRefusal bool) (*clusters.GRPCClient, context.Context) {
	md := metadata.MD{}

	if n.IsClusterNode() {
		md = edge.SerializeSandboxCatalogDeleteEvent(
			edge.SandboxCatalogDeleteEvent{
				SandboxID:        sandboxID,
				ExecutionID:      executionID,
				RestoreOnRefusal: restoreOnRefusal,
			},
		)
	}

	// Merge medata from client (auth, routing with service instance id) and event metadata.
	return n.client, appendMetadataCtx(ctx, md)
}

func appendMetadataCtx(ctx context.Context, md metadata.MD) context.Context {
	args := make([]string, 0, len(md)*2)
	for k, v := range md {
		args = append(args, k, v[0])
	}

	return metadata.AppendToOutgoingContext(ctx, args...)
}
```

`packages/api/internal/orchestrator/nodemanager/sandbox_create.go` lines 1-13 (of 13)

```go
package nodemanager

import (
	"context"

	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

func (n *Node) SandboxCreate(ctx context.Context, sbxRequest *orchestrator.SandboxCreateRequest) (*orchestrator.SandboxCreateResponse, error) {
	client, ctx := n.GetSandboxCreateCtx(ctx, sbxRequest)

	return client.Sandbox.Create(ctx, sbxRequest)
}
```

---

## 2. envd protos and HTTP OpenAPI

### 2.1 `packages/envd/spec/process/process.proto` (complete)

Proto package `process` so Connect procedure paths are `/process.Process/<Method>`.

`packages/envd/spec/process/process.proto` lines 1-171 (of 171)

```proto
syntax = "proto3";

package process;

service Process {
    rpc List(ListRequest) returns (ListResponse);

    rpc Connect(ConnectRequest) returns (stream ConnectResponse);
    rpc Start(StartRequest) returns (stream StartResponse);

    rpc Update(UpdateRequest) returns (UpdateResponse);

    // Client input stream ensures ordering of messages
    rpc StreamInput(stream StreamInputRequest) returns (StreamInputResponse);
    rpc SendInput(SendInputRequest) returns (SendInputResponse);
    rpc SendSignal(SendSignalRequest) returns (SendSignalResponse);

    // Close stdin to signal EOF to the process.
    // Only works for non-PTY processes. For PTY, send Ctrl+D (0x04) instead.
    rpc CloseStdin(CloseStdinRequest) returns (CloseStdinResponse);
}

message PTY {
    Size size = 1;

    message Size {
        uint32 cols = 1;
        uint32 rows = 2;
    }
}

message ProcessConfig {
    string cmd = 1;
    repeated string args = 2;
    
    map<string, string> envs = 3;
    optional string cwd = 4;
}

message ListRequest {}

message ProcessInfo {
    ProcessConfig config = 1;
    uint32 pid = 2;
    optional string tag = 3;
}

message ListResponse {
    repeated ProcessInfo processes = 1;
}

message StartRequest {    
    ProcessConfig process = 1;
    optional PTY pty = 2;
    optional string tag = 3;
    // This is optional for backwards compatibility.
    // We default to true. New SDK versions will set this to false by default.
    optional bool stdin = 4;
}

message UpdateRequest {
    ProcessSelector process = 1;

    optional PTY pty = 2;
}

message UpdateResponse {}

message ProcessEvent {
    oneof event {
        StartEvent start = 1;
        DataEvent data = 2;
        EndEvent end = 3;
        KeepAlive keepalive = 4;
    }
    
    message StartEvent {
        uint32 pid = 1;
    }
    
    message DataEvent {
        oneof output {
            bytes stdout = 1;
            bytes stderr = 2;
            bytes pty = 3;
        }
    }
    
    message EndEvent {
        sint32 exit_code = 1;
        bool exited = 2;
        string status = 3;
        optional string error = 4;
    }

    message KeepAlive {}
}

message StartResponse {
    ProcessEvent event = 1;
}

message ConnectResponse {
    ProcessEvent event = 1;
}

message SendInputRequest {
    ProcessSelector process = 1;

    ProcessInput input = 2;
}

message SendInputResponse {}

message ProcessInput {
    oneof input {
        bytes stdin = 1;
        bytes pty = 2;
    }
}

message StreamInputRequest {
    oneof event {
        StartEvent start = 1;
        DataEvent data = 2;
        KeepAlive keepalive = 3;
    }

    message StartEvent {
        ProcessSelector process = 1;
    }

    message DataEvent {
        ProcessInput input = 2;
    }

    message KeepAlive {}
}

message StreamInputResponse {}

enum Signal {
    SIGNAL_UNSPECIFIED = 0;
    SIGNAL_SIGTERM = 15;
    SIGNAL_SIGKILL = 9;
}

message SendSignalRequest {
    ProcessSelector process = 1;

    Signal signal = 2;
}

message SendSignalResponse {}

message CloseStdinRequest {
    ProcessSelector process = 1;
}

message CloseStdinResponse {}

message ConnectRequest {
    ProcessSelector process = 1;
}

message ProcessSelector {
    oneof selector {
        uint32 pid = 1;
        string tag = 2;
    }
}
```

### 2.2 `packages/envd/spec/filesystem/filesystem.proto` (complete)

Proto package `filesystem`; procedure paths `/filesystem.Filesystem/<Method>`.

`packages/envd/spec/filesystem/filesystem.proto` lines 1-155 (of 155)

```proto
syntax = "proto3";

package filesystem;

import "google/protobuf/timestamp.proto";

service Filesystem {
  rpc Stat(StatRequest) returns (StatResponse);
  rpc MakeDir(MakeDirRequest) returns (MakeDirResponse);
  rpc Move(MoveRequest) returns (MoveResponse);
  rpc ListDir(ListDirRequest) returns (ListDirResponse);
  rpc Remove(RemoveRequest) returns (RemoveResponse);

  rpc WatchDir(WatchDirRequest) returns (stream WatchDirResponse);

  // Non-streaming versions of WatchDir
  rpc CreateWatcher(CreateWatcherRequest) returns (CreateWatcherResponse);
  rpc GetWatcherEvents(GetWatcherEventsRequest) returns (GetWatcherEventsResponse);
  rpc RemoveWatcher(RemoveWatcherRequest) returns (RemoveWatcherResponse);
}

message MoveRequest {
  string source = 1;
  string destination = 2;
}

message MoveResponse {
  EntryInfo entry = 1;
}

message MakeDirRequest {
  string path = 1;
}

message MakeDirResponse {
  EntryInfo entry = 1;
}

message RemoveRequest {
  string path = 1;
}

message RemoveResponse {}

message StatRequest {
  string path = 1;
}

message StatResponse {
  EntryInfo entry = 1;
}

message EntryInfo {
  string name = 1;
  FileType type = 2;
  string path = 3;
  int64 size = 4;
  uint32 mode = 5;
  string permissions = 6;
  string owner = 7;
  string group = 8;
  google.protobuf.Timestamp modified_time = 9;
  // If the entry is a symlink, this field contains the target of the symlink.
  optional string symlink_target = 10;
  // User-defined metadata stored as extended attributes (xattrs) on the file.
  // Keys live under the `user.e2b.` xattr namespace; the prefix is stripped here.
  // Plain `user.*` xattrs written by other tooling are not reflected.
  map<string, string> metadata = 11;
  // True when the entry itself is a symlink; type then describes the symlink's target.
  bool is_symlink = 12;
}

enum FileType {
  FILE_TYPE_UNSPECIFIED = 0;
  FILE_TYPE_FILE = 1;
  FILE_TYPE_DIRECTORY = 2;
  FILE_TYPE_SYMLINK = 3;
}

message ListDirRequest {
  string path = 1;
  uint32 depth = 2;
}

message ListDirResponse {
  repeated EntryInfo entries = 1;
}

message WatchDirRequest {
  string path = 1;
  bool recursive = 2;
  // If true, each FilesystemEvent includes the EntryInfo of the affected entry, when available.
  bool include_entry = 3;
  // If true, allows watching paths on network filesystem mounts (NFS, CIFS, SMB, FUSE).
  // Events on network mounts may be unreliable or not delivered at all.
  bool allow_network_mounts = 4;
}

message FilesystemEvent {
  string name = 1;
  EventType type = 2;
  // Info of the entry that triggered the event. Only populated when include_entry
  // was requested and the entry could be stat-ed (e.g. not set for remove/rename-away
  // events, where the entry no longer exists at this path).
  optional EntryInfo entry = 3;
}

message WatchDirResponse {
  oneof event {
    StartEvent start = 1;
    FilesystemEvent filesystem = 2;
    KeepAlive keepalive = 3;
  }

  message StartEvent {}

  message KeepAlive {}
}

message CreateWatcherRequest {
  string path = 1;
  bool recursive = 2;
  // If true, each FilesystemEvent includes the EntryInfo of the affected entry, when available.
  bool include_entry = 3;
  // If true, allows watching paths on network filesystem mounts (NFS, CIFS, SMB, FUSE).
  // Events on network mounts may be unreliable or not delivered at all.
  bool allow_network_mounts = 4;
}

message CreateWatcherResponse {
  string watcher_id = 1;
}

message GetWatcherEventsRequest {
  string watcher_id = 1;
}

message GetWatcherEventsResponse {
  repeated FilesystemEvent events = 1;
}

message RemoveWatcherRequest {
  string watcher_id = 1;
}

message RemoveWatcherResponse {}

enum EventType {
  EVENT_TYPE_UNSPECIFIED = 0;
  EVENT_TYPE_CREATE = 1;
  EVENT_TYPE_WRITE = 2;
  EVENT_TYPE_REMOVE = 3;
  EVENT_TYPE_RENAME = 4;
  EVENT_TYPE_CHMOD = 5;
}
```

### 2.3 `packages/envd/spec/envd.yaml` (complete OpenAPI; includes /init, /health, /files in full)

Note `x-internal: true` on /init, /freeze, /unfreeze, /collapse, /fsfreeze, /fsthaw: the orchestrator
proxy refuses these paths from outside (see §5.3). /init is called by the orchestrator itself at the slot
IP (see §4.6); an external client never calls it.

`packages/envd/spec/envd.yaml` lines 1-643 (of 643)

```yaml
openapi: 3.0.0
info:
  title: envd
  version: 0.1.3
  description: API for managing files' content and controlling envd

tags:
  - name: files

# Operations marked `x-internal: true` are the orchestrator's control plane. The
# orchestrator reaches them over the host network, straight at the sandbox slot
# IP, so they are never a legitimate destination for traffic arriving through the
# public sandbox URL. The sandbox proxy generates its rejection list from this
# marker, so adding a control operation here is what closes it to the outside;
# see the orchestrator's `pkg/sandbox/envd` package.
paths:
  /health:
    get:
      summary: Check the health of the service
      responses:
        "204":
          description: The service is healthy

  /metrics:
    get:
      summary: Service stats
      security:
        - AccessTokenAuth: []
        - {}
      responses:
        "200":
          description: The resource usage metrics of the service
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/Metrics"

  /init:
    post:
      summary: Set initial vars, ensure the time and metadata is synced with the host
      x-internal: true
      security:
        - AccessTokenAuth: []
        - {}
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                volumeMounts:
                  type: array
                  items:
                    $ref: "#/components/schemas/VolumeMount"
                hyperloopIP:
                  type: string
                  description: IP address of the hyperloop server to connect to
                lifecycleID:
                  type: string
                  description: Lifecycle ID of the sandbox
                envVars:
                  $ref: "#/components/schemas/EnvVars"
                accessToken:
                  type: string
                  description: Access token for secure access to envd service
                  x-go-type: SecureToken
                timestamp:
                  type: string
                  format: date-time
                  description: The current timestamp in RFC3339 format
                defaultUser:
                  type: string
                  description: The default user to use for operations
                defaultWorkdir:
                  type: string
                  description: The default working directory to use for operations
                caBundle:
                  type: string
                  description: PEM-encoded CA certificates to install into the system trust store (may contain multiple concatenated PEM blocks)

      responses:
        "204":
          description: Env vars set, the time and metadata is synced with the host

  /freeze:
    post:
      summary: Freeze user/pty cgroups before pause and wait for them to stop. Written directly by envd to avoid Process.Start / shell overhead under load.
      description: |
        Writing cgroup.freeze only requests a freeze; the kernel stops each task at its
        next signal-delivery point. This endpoint therefore polls cgroup.events until the
        cgroups read back frozen, and returns what it observed, so a caller can tell a
        freeze that took effect from one that was merely issued.

        cgroup.events reports STATE, not an acknowledgement of our write: a cgroup the
        guest froze itself reads frozen too. The counts below say what was observed, not
        what this call caused.

        Best-effort by design: a workload that will not quiesce within the budget is
        reported as unconfirmed rather than failing the call, cgroups that reject the write
        are counted in failed, and cgroups the guest removed while the sweep was working on
        them are counted in vanished. None of the three may block a pause, which is why all
        three are counts in the body rather than an error.

        Whether this endpoint waits is the caller's choice, expressed by supplying
        maxWaitMs: see that parameter.
      x-internal: true
      parameters:
        - name: mode
          in: query
          required: false
          description: |
            Which cgroups to freeze. "hierarchy" freezes the complement of envd's own
            ancestor chain, so cgroups the customer created anywhere in the tree are
            covered; "legacy" freezes only the user and pty cgroups envd itself creates.
            Omitted means legacy, which is what an orchestrator predating this parameter
            gets.

            The mode is chosen by the caller because the feature flag that selects it is
            evaluated there — envd has no access to it. FreezeResult echoes the mode back
            so the caller can confirm envd honoured the request rather than inferring it
            from the flag's value: an envd too old to know about modes reports legacy
            while the flag reads on.
          schema:
            type: string
            enum: [legacy, hierarchy]
        - name: maxCgroups
          in: query
          required: false
          description: |
            Bounds how many cgroups a hierarchy sweep may visit. A safety guard against a
            pathological or hostile hierarchy rather than a performance knob — the guest is
            the threat model. Omitted or non-positive means envd's own default. Ignored in
            legacy mode.
          schema:
            type: integer
        - name: maxWaitMs
          in: query
          required: false
          description: |
            How long to wait for the cgroups to read back frozen, in milliseconds. The
            caller owns this budget because it also owns the request timeout, and a wait
            longer than that timeout cannot be observed.

            Supplying it also selects the response: with it, the call waits and answers 200
            with a FreezeResult; omitted or non-positive, the call does not wait at all and
            answers 204, which is the contract callers older than this parameter expect.
          schema:
            type: integer
            format: int64
      security:
        - AccessTokenAuth: []
        - {}
      responses:
        "200":
          description: Freeze issued and the frozen state awaited (maxWaitMs was supplied); the body reports what was observed
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/FreezeResult"
        "204":
          description: Freeze issued without waiting (maxWaitMs absent), for callers predating the structured result
        "500":
          $ref: "#/components/responses/InternalServerError"
        "503":
          description: Freeze lock is held by another operation

  /unfreeze:
    post:
      summary: Unfreeze user/pty cgroups. Intended ONLY for the orchestrator's pause-failure rollback path; the normal resume thaw happens via /init's deferred unfreeze, not here.
      x-internal: true
      security:
        - AccessTokenAuth: []
        - {}
      responses:
        "204":
          description: Cgroups unfrozen
        "500":
          $ref: "#/components/responses/InternalServerError"
        "503":
          description: Freeze lock is held by another operation

  /collapse:
    post:
      summary: Collapse envd's own anonymous heap into 2 MiB transparent hugepages before pause, so on resume envd touches fewer distinct guest-physical frames (each a cold fault). Best-effort.
      x-internal: true
      security:
        - AccessTokenAuth: []
        - {}
      responses:
        "200":
          description: Heap collapsed
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/CollapseResult"
        "500":
          $ref: "#/components/responses/InternalServerError"

  /fsfreeze:
    post:
      summary: Freeze the guest rootfs (FIFREEZE) before a filesystem-only pause so it is flushed to a consistent on-disk state, closing the sync->pause race. Idempotent. On a successful filesystem-only pause the VM is rebooted, so no thaw is needed; the orchestrator thaws only on the pause-failure path.
      x-internal: true
      security:
        - AccessTokenAuth: []
        - {}
      responses:
        "204":
          description: Rootfs frozen
        "500":
          $ref: "#/components/responses/InternalServerError"
        "503":
          description: Freeze lock is held by another operation

  /fsthaw:
    post:
      summary: Thaw the guest rootfs (FITHAW). Intended ONLY for the orchestrator's pause-failure rollback path, so a frozen filesystem cannot leave the live VM deadlocked. Idempotent.
      x-internal: true
      security:
        - AccessTokenAuth: []
        - {}
      responses:
        "204":
          description: Rootfs thawed
        "500":
          $ref: "#/components/responses/InternalServerError"
        "503":
          description: Freeze lock is held by another operation

  /envs:
    get:
      summary: Environment variables
      security:
        - AccessTokenAuth: []
        - {}
      responses:
        "200":
          description: Environment variables
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/EnvVars"

  /files:
    get:
      summary: Download a file
      tags: [files]
      security:
        - AccessTokenAuth: []
        - {}
      parameters:
        - $ref: "#/components/parameters/FilePath"
        - $ref: "#/components/parameters/User"
        - $ref: "#/components/parameters/Signature"
        - $ref: "#/components/parameters/SignatureExpiration"
      responses:
        "200":
          $ref: "#/components/responses/DownloadSuccess"
        "400":
          $ref: "#/components/responses/InvalidPath"
        "401":
          $ref: "#/components/responses/InvalidUser"
        "404":
          $ref: "#/components/responses/FileNotFound"
        "406":
          $ref: "#/components/responses/NotAcceptable"
        "500":
          $ref: "#/components/responses/InternalServerError"
    post:
      summary: Upload a file and ensure the parent directories exist. If the file exists, it will be overwritten.
      description: |
        Any request header of the form `X-Metadata-<key>: <value>` is persisted
        as a user-defined extended attribute on the uploaded file. The
        `X-Metadata-` prefix is stripped and the remaining header name is
        lowercased to form the metadata key; the resulting map is returned on
        `EntryInfo` lookups (e.g. `Stat`, `ListDir`).

        Each upload replaces the file's metadata with the keys provided in
        that request: keys previously stored but absent from the new request
        are removed, and an upload that sends no `X-Metadata-*` header clears
        all existing metadata.

        Both keys and values must be printable US-ASCII (bytes `0x20`-`0x7E`)
        and are rejected with HTTP 400 otherwise. Each key is capped at 246
        bytes (the Linux VFS xattr-name limit minus the namespace prefix), and
        the combined size of all metadata on a file (keys plus values, with the
        namespace prefix counted per key) is capped at 4096 bytes to stay within
        the filesystem's per-inode xattr budget. Multiple files in a single
        multipart upload receive the same metadata. If the same
        `X-Metadata-<key>` header is sent more than once, only the first
        value is used.
      tags: [files]
      security:
        - AccessTokenAuth: []
        - {}
      parameters:
        - $ref: "#/components/parameters/FilePath"
        - $ref: "#/components/parameters/User"
        - $ref: "#/components/parameters/Signature"
        - $ref: "#/components/parameters/SignatureExpiration"
      requestBody:
        $ref: "#/components/requestBodies/File"
      responses:
        "200":
          $ref: "#/components/responses/UploadSuccess"
        "400":
          $ref: "#/components/responses/InvalidPath"
        "401":
          $ref: "#/components/responses/InvalidUser"
        "500":
          $ref: "#/components/responses/InternalServerError"
        "507":
          $ref: "#/components/responses/NotEnoughDiskSpace"

  /files/compose:
    post:
      summary: Compose multiple files into a single file using zero-copy concatenation. Source files are deleted after successful composition.
      tags: [files]
      security:
        - AccessTokenAuth: []
        - {}
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: "#/components/schemas/ComposeRequest"
      responses:
        "200":
          description: Files composed successfully
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/EntryInfo"
        "400":
          $ref: "#/components/responses/InvalidPath"
        "401":
          $ref: "#/components/responses/InvalidUser"
        "404":
          $ref: "#/components/responses/FileNotFound"
        "500":
          $ref: "#/components/responses/InternalServerError"
        "507":
          $ref: "#/components/responses/NotEnoughDiskSpace"

components:
  securitySchemes:
    AccessTokenAuth:
      type: apiKey
      in: header
      name: X-Access-Token

  parameters:
    FilePath:
      name: path
      in: query
      required: false
      description: Path to the file, URL encoded. Can be relative to the user's home directory (e.g. "file.txt" resolves to ~/file.txt).
      schema:
        type: string
    User:
      name: username
      in: query
      required: false
      description: User for setting file ownership and resolving relative paths. Defaults to the sandbox's default user.
      schema:
        type: string
    Signature:
      name: signature
      in: query
      required: false
      description: Signature used for file access permission verification.
      schema:
        type: string
    SignatureExpiration:
      name: signature_expiration
      in: query
      required: false
      description: Unix timestamp (seconds) after which the signature expires. Only used with the signature parameter.
      schema:
        type: integer
  requestBodies:
    File:
      required: true
      content:
        multipart/form-data:
          schema:
            type: object
            properties:
              file:
                type: string
                format: binary
        application/octet-stream:
          schema:
            type: string
            format: binary
            description: Raw file content. The 'path' query parameter is required when using this content type.

  responses:
    UploadSuccess:
      description: The file was uploaded successfully.
      content:
        application/json:
          schema:
            type: array
            items:
              $ref: "#/components/schemas/EntryInfo"
          example:
            - path: "/home/user/hello.txt"
              name: "hello.txt"
              type: "file"

    DownloadSuccess:
      description: Entire file downloaded successfully.
      content:
        application/octet-stream:
          schema:
            type: string
            format: binary
            description: The raw file content
    NotAcceptable:
      description: Requested encoding is not supported
      content:
        application/json:
          schema:
            $ref: "#/components/schemas/Error"
          example:
            message: "no acceptable encoding found, supported: [identity, gzip]"
            code: 406
    InvalidPath:
      description: Invalid path
      content:
        application/json:
          schema:
            $ref: "#/components/schemas/Error"
          example:
            message: "path '/home/user/docs' is a directory"
            code: 400
    InternalServerError:
      description: Internal server error
      content:
        application/json:
          schema:
            $ref: "#/components/schemas/Error"
          example:
            message: "error opening file '/home/user/file.txt': permission denied"
            code: 500
    FileNotFound:
      description: File not found
      content:
        application/json:
          schema:
            $ref: "#/components/schemas/Error"
          example:
            message: "path '/home/user/missing.txt' does not exist"
            code: 404
    InvalidUser:
      description: Invalid user
      content:
        application/json:
          schema:
            $ref: "#/components/schemas/Error"
          example:
            message: "error looking up user 'nonexistent': user: unknown user nonexistent"
            code: 401
    NotEnoughDiskSpace:
      description: Not enough disk space
      content:
        application/json:
          schema:
            $ref: "#/components/schemas/Error"
          example:
            message: "not enough disk space available"
            code: 507

  schemas:
    Error:
      required:
        - message
        - code
      properties:
        message:
          type: string
          description: Error message
        code:
          type: integer
          description: Error code
    EntryInfo:
      required:
        - path
        - name
        - type
      properties:
        path:
          type: string
          description: Path to the file
        name:
          type: string
          description: Name of the file
        type:
          type: string
          description: Type of the file
          enum:
            - file
        metadata:
          type: object
          description: User-defined metadata stored as extended attributes on the file.
          additionalProperties:
            type: string
    EnvVars:
      type: object
      description: Environment variables to set
      additionalProperties:
        type: string
    Metrics:
      type: object
      description: Resource usage metrics
      properties:
        ts:
          type: integer
          format: int64
          description: Unix timestamp in UTC for current sandbox time
        cpu_count:
          type: integer
          description: Number of CPU cores
        cpu_used_pct:
          type: number
          format: float
          description: CPU usage percentage
        mem_total:
          type: integer
          description: Total virtual memory in bytes
        mem_used:
          type: integer
          description: Used virtual memory in bytes
        mem_cache:
          type: integer
          description: Cached memory (page cache) in bytes
        disk_used:
          type: integer
          description: Used disk space in bytes
        disk_total:
          type: integer
          description: Total disk space in bytes
    FreezeResult:
      type: object
      description: Per-call statistics from a pre-pause workload freeze
      properties:
        mode:
          type: string
          enum: [legacy, hierarchy]
          description: Which sweep actually ran. Echoed back rather than inferred from the flag, so a caller can tell that envd honoured what it asked for
        visited:
          type: integer
          description: Cgroups the walk examined, whether or not it froze them. The input for sizing the bound; meaningless in legacy mode
        allowlisted:
          type: integer
          description: Cgroups skipped because the resume path depends on them (systemd, journald, envd's port forwarding). Reported because the allowlist is expected to grow, and a distro that routes journald differently changes this count
        truncated:
          type: boolean
          description: True when the walk stopped because it hit the bound rather than because it ran out of tree, so coverage is incomplete
        preFrozen:
          type: integer
          description: Cgroups the guest itself had already frozen before the sweep ran (docker pause writes cgroup.freeze). Not written to and deliberately left frozen by the resume thaw, so the guest's own suspension survives the snapshot
        requested:
          type: integer
          description: Cgroups this call wrote cgroup.freeze to
        frozen:
          type: integer
          description: Cgroups that read back "frozen 1" from cgroup.events within the budget; their tasks have stopped
        notFrozen:
          type: integer
          description: Cgroups still reading "frozen 0" when the budget expired; their tasks may still be running, so a snapshot taken now can capture a live workload
        failed:
          type: integer
          description: Cgroups that errored for a reason that is not simply being gone - the write was refused, or the state could not be read. A cgroup the hierarchy walk discovered that merely went away is counted vanished instead; one of envd's own static cgroups that goes away is counted here, by design
        vanished:
          type: integer
          description: Cgroups the hierarchy walk enumerated that the guest then removed before the sweep finished with them - envd's own static cgroups are excluded and report failed instead. A race rather than a failure, and a claim about the cgroup only - tasks migrated out of it before its removal can still be running. Like failed it spans both phases, so it does not reconcile against requested on its own - one removed during the settle poll was counted in requested, one removed before its write never was
        unobservable:
          type: integer
          description: Cgroups whose freeze state cannot be read because this guest has no cgroup manager; the write was accepted but nothing can be read back, so these are neither frozen nor notFrozen
        sweepMs:
          type: integer
          format: int64
          description: Time spent issuing the freeze writes, in milliseconds (scales with cgroup count)
        waitMs:
          type: integer
          format: int64
          description: Time spent polling cgroup.events, in milliseconds (scales with how deep in I/O the guest tasks were). Outcome neutral - the wait ends either because everything stopped or because the budget ran out
    CollapseResult:
      type: object
      description: Per-call statistics from a heap collapse
      properties:
        regions:
          type: integer
          description: Anonymous read-write regions scanned
        chunks:
          type: integer
          description: 2 MiB chunks attempted
        collapsed:
          type: integer
          description: Chunks whose base pages were actually migrated into a new hugepage (real work)
        alreadyHuge:
          type: integer
          description: Chunks MADV_COLLAPSE accepted but were already hugepages (no work)
        skipped:
          type: integer
          description: Chunks that could not be collapsed (empty or ineligible)
        elapsedMs:
          type: integer
          format: int64
          description: Wall-clock time spent collapsing, in milliseconds
    ComposeRequest:
      type: object
      required:
        - source_paths
        - destination
      properties:
        source_paths:
          type: array
          items:
            type: string
          description: Ordered list of source file paths to concatenate
        destination:
          type: string
          description: Destination file path for the composed file
        username:
          type: string
          description: User for setting ownership and resolving relative paths
    VolumeMount:
      type: object
      description: Volume mount configuration
      additionalProperties: false
      properties:
        nfs_target:
          type: string
          description: Server target address
        path:
          type: string
          description: Mount path inside the sandbox
      required:
        - nfs_target
        - path
```

### 2.4 Where the generated envd Go Connect clients live

Generated with buf into the shared module (managed `go_package_prefix`), and a second copy into envd's
own internal tree for the server:

`packages/envd/spec/generate.go` lines 1-4 (of 4)

```go
package spec

//go:generate buf generate --template buf.gen.yaml
//go:generate buf generate --template buf.gen.shared.yaml
```

`packages/envd/spec/buf.gen.shared.yaml` lines 1-14 (of 14)

```yaml
version: v1
plugins:
  - plugin: go
    out: ../../shared/pkg/grpc/envd
    opt: paths=source_relative
  - plugin: connect-go
    out: ../../shared/pkg/grpc/envd
    opt: paths=source_relative

managed:
  enabled: true
  optimize_for: SPEED
  go_package_prefix:
    default: github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd
```

`packages/envd/spec/buf.gen.yaml` lines 1-14 (of 14)

```yaml
version: v1
plugins:
  - plugin: go
    out: ../internal/services/spec
    opt: paths=source_relative
  - plugin: connect-go
    out: ../internal/services/spec
    opt: paths=source_relative

managed:
  enabled: true
  optimize_for: SPEED
  go_package_prefix:
    default: github.com/e2b-dev/infra/packages/envd/internal/services/spec
```

Client-side packages (importable):
- `github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process` (package `process`), Connect client in `.../process/processconnect` (package `processconnect`)
- `github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/filesystem` (package `filesystem`), Connect client in `.../filesystem/filesystemconnect` (package `filesystemconnect`)

Server-side copies (not importable from outside the envd module; `internal/`):
`packages/envd/internal/services/spec/{process,filesystem,upgrade}`.

The HTTP (OpenAPI) client types are generated by oapi-codegen into
`packages/orchestrator/pkg/sandbox/envd/envd.gen.go` (package `envd`, orchestrator module) and
`packages/envd/internal/api/api.gen.go` (server); the integration tests have a client at
`tests/integration/internal/envd` (internal).

`packages/orchestrator/pkg/sandbox/envd/generate.go` lines 1-4 (of 4)

```go
package envd

//go:generate go tool github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen -config cfg.yaml ../../../../envd/spec/envd.yaml
//go:generate go run gen_internal_routes.go
```

`packages/shared/pkg/grpc/envd/process/processconnect/process.connect.go` lines 1-80 (of 310)

```go
// Code generated by protoc-gen-connect-go. DO NOT EDIT.
//
// Source: process/process.proto

package processconnect

import (
	connect "connectrpc.com/connect"
	context "context"
	errors "errors"
	process "github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process"
	http "net/http"
	strings "strings"
)

// This is a compile-time assertion to ensure that this generated file and the connect package are
// compatible. If you get a compiler error that this constant is not defined, this code was
// generated with a version of connect newer than the one compiled into your binary. You can fix the
// problem by either regenerating this code with an older version of connect or updating the connect
// version compiled into your binary.
const _ = connect.IsAtLeastVersion1_13_0

const (
	// ProcessName is the fully-qualified name of the Process service.
	ProcessName = "process.Process"
)

// These constants are the fully-qualified names of the RPCs defined in this package. They're
// exposed at runtime as Spec.Procedure and as the final two segments of the HTTP route.
//
// Note that these are different from the fully-qualified method names used by
// google.golang.org/protobuf/reflect/protoreflect. To convert from these constants to
// reflection-formatted method names, remove the leading slash and convert the remaining slash to a
// period.
const (
	// ProcessListProcedure is the fully-qualified name of the Process's List RPC.
	ProcessListProcedure = "/process.Process/List"
	// ProcessConnectProcedure is the fully-qualified name of the Process's Connect RPC.
	ProcessConnectProcedure = "/process.Process/Connect"
	// ProcessStartProcedure is the fully-qualified name of the Process's Start RPC.
	ProcessStartProcedure = "/process.Process/Start"
	// ProcessUpdateProcedure is the fully-qualified name of the Process's Update RPC.
	ProcessUpdateProcedure = "/process.Process/Update"
	// ProcessStreamInputProcedure is the fully-qualified name of the Process's StreamInput RPC.
	ProcessStreamInputProcedure = "/process.Process/StreamInput"
	// ProcessSendInputProcedure is the fully-qualified name of the Process's SendInput RPC.
	ProcessSendInputProcedure = "/process.Process/SendInput"
	// ProcessSendSignalProcedure is the fully-qualified name of the Process's SendSignal RPC.
	ProcessSendSignalProcedure = "/process.Process/SendSignal"
	// ProcessCloseStdinProcedure is the fully-qualified name of the Process's CloseStdin RPC.
	ProcessCloseStdinProcedure = "/process.Process/CloseStdin"
)

// ProcessClient is a client for the process.Process service.
type ProcessClient interface {
	List(context.Context, *connect.Request[process.ListRequest]) (*connect.Response[process.ListResponse], error)
	Connect(context.Context, *connect.Request[process.ConnectRequest]) (*connect.ServerStreamForClient[process.ConnectResponse], error)
	Start(context.Context, *connect.Request[process.StartRequest]) (*connect.ServerStreamForClient[process.StartResponse], error)
	Update(context.Context, *connect.Request[process.UpdateRequest]) (*connect.Response[process.UpdateResponse], error)
	// Client input stream ensures ordering of messages
	StreamInput(context.Context) *connect.ClientStreamForClient[process.StreamInputRequest, process.StreamInputResponse]
	SendInput(context.Context, *connect.Request[process.SendInputRequest]) (*connect.Response[process.SendInputResponse], error)
	SendSignal(context.Context, *connect.Request[process.SendSignalRequest]) (*connect.Response[process.SendSignalResponse], error)
	// Close stdin to signal EOF to the process.
	// Only works for non-PTY processes. For PTY, send Ctrl+D (0x04) instead.
	CloseStdin(context.Context, *connect.Request[process.CloseStdinRequest]) (*connect.Response[process.CloseStdinResponse], error)
}

// NewProcessClient constructs a client for the process.Process service. By default, it uses the
// Connect protocol with the binary Protobuf Codec, asks for gzipped responses, and sends
// uncompressed requests. To use the gRPC or gRPC-Web protocols, supply the connect.WithGRPC() or
// connect.WithGRPCWeb() options.
//
// The URL supplied here should be the base URL for the Connect or gRPC server (for example,
// http://api.acme.com or https://acme.com/grpc).
func NewProcessClient(httpClient connect.HTTPClient, baseURL string, opts ...connect.ClientOption) ProcessClient {
	baseURL = strings.TrimRight(baseURL, "/")
	processMethods := process.File_process_process_proto.Services().ByName("Process").Methods()
	return &processClient{
		list: connect.NewClient[process.ListRequest, process.ListResponse](
```

`packages/shared/pkg/grpc/envd/filesystem/filesystemconnect/filesystem.connect.go` lines 30-81 (of 337)

```go
//
// Note that these are different from the fully-qualified method names used by
// google.golang.org/protobuf/reflect/protoreflect. To convert from these constants to
// reflection-formatted method names, remove the leading slash and convert the remaining slash to a
// period.
const (
	// FilesystemStatProcedure is the fully-qualified name of the Filesystem's Stat RPC.
	FilesystemStatProcedure = "/filesystem.Filesystem/Stat"
	// FilesystemMakeDirProcedure is the fully-qualified name of the Filesystem's MakeDir RPC.
	FilesystemMakeDirProcedure = "/filesystem.Filesystem/MakeDir"
	// FilesystemMoveProcedure is the fully-qualified name of the Filesystem's Move RPC.
	FilesystemMoveProcedure = "/filesystem.Filesystem/Move"
	// FilesystemListDirProcedure is the fully-qualified name of the Filesystem's ListDir RPC.
	FilesystemListDirProcedure = "/filesystem.Filesystem/ListDir"
	// FilesystemRemoveProcedure is the fully-qualified name of the Filesystem's Remove RPC.
	FilesystemRemoveProcedure = "/filesystem.Filesystem/Remove"
	// FilesystemWatchDirProcedure is the fully-qualified name of the Filesystem's WatchDir RPC.
	FilesystemWatchDirProcedure = "/filesystem.Filesystem/WatchDir"
	// FilesystemCreateWatcherProcedure is the fully-qualified name of the Filesystem's CreateWatcher
	// RPC.
	FilesystemCreateWatcherProcedure = "/filesystem.Filesystem/CreateWatcher"
	// FilesystemGetWatcherEventsProcedure is the fully-qualified name of the Filesystem's
	// GetWatcherEvents RPC.
	FilesystemGetWatcherEventsProcedure = "/filesystem.Filesystem/GetWatcherEvents"
	// FilesystemRemoveWatcherProcedure is the fully-qualified name of the Filesystem's RemoveWatcher
	// RPC.
	FilesystemRemoveWatcherProcedure = "/filesystem.Filesystem/RemoveWatcher"
)

// FilesystemClient is a client for the filesystem.Filesystem service.
type FilesystemClient interface {
	Stat(context.Context, *connect.Request[filesystem.StatRequest]) (*connect.Response[filesystem.StatResponse], error)
	MakeDir(context.Context, *connect.Request[filesystem.MakeDirRequest]) (*connect.Response[filesystem.MakeDirResponse], error)
	Move(context.Context, *connect.Request[filesystem.MoveRequest]) (*connect.Response[filesystem.MoveResponse], error)
	ListDir(context.Context, *connect.Request[filesystem.ListDirRequest]) (*connect.Response[filesystem.ListDirResponse], error)
	Remove(context.Context, *connect.Request[filesystem.RemoveRequest]) (*connect.Response[filesystem.RemoveResponse], error)
	WatchDir(context.Context, *connect.Request[filesystem.WatchDirRequest]) (*connect.ServerStreamForClient[filesystem.WatchDirResponse], error)
	// Non-streaming versions of WatchDir
	CreateWatcher(context.Context, *connect.Request[filesystem.CreateWatcherRequest]) (*connect.Response[filesystem.CreateWatcherResponse], error)
	GetWatcherEvents(context.Context, *connect.Request[filesystem.GetWatcherEventsRequest]) (*connect.Response[filesystem.GetWatcherEventsResponse], error)
	RemoveWatcher(context.Context, *connect.Request[filesystem.RemoveWatcherRequest]) (*connect.Response[filesystem.RemoveWatcherResponse], error)
}

// NewFilesystemClient constructs a client for the filesystem.Filesystem service. By default, it
// uses the Connect protocol with the binary Protobuf Codec, asks for gzipped responses, and sends
// uncompressed requests. To use the gRPC or gRPC-Web protocols, supply the connect.WithGRPC() or
// connect.WithGRPCWeb() options.
//
// The URL supplied here should be the base URL for the Connect or gRPC server (for example,
// http://api.acme.com or https://acme.com/grpc).
func NewFilesystemClient(httpClient connect.HTTPClient, baseURL string, opts ...connect.ClientOption) FilesystemClient {
	baseURL = strings.TrimRight(baseURL, "/")
```

---

## 3. How the E2B API calls the orchestrator

### 3.1 Access tokens: `packages/api/internal/sandbox/sandbox_envd_secret.go` (complete) and the hasher

`seedKey` is the API's configured secret. envd token = hex(HMAC-SHA256(seed, sandboxID));
traffic token = hex(HMAC-SHA256(seed, "sandbox-traffic-" + sandboxID)). Any unguessable string works
for a re-implementation: the orchestrator only forwards and compares it.

`packages/api/internal/sandbox/sandbox_envd_secret.go` lines 1-35 (of 35)

```go
package sandbox

import (
	"errors"
	"fmt"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/shared/pkg/keys"
)

const sandboxTrafficPrefix = "sandbox-traffic"

type AccessTokenGenerator struct {
	hasher *keys.HMACSha256Hashing
}

func NewAccessTokenGenerator(seedKey string) (*AccessTokenGenerator, error) {
	if seedKey == "" {
		return nil, errors.New("seed key is not set")
	}

	return &AccessTokenGenerator{
		hasher: keys.NewHMACSHA256Hashing([]byte(seedKey)),
	}, nil
}

func (g *AccessTokenGenerator) GenerateEnvdAccessToken(id api.SandboxID) (string, error) {
	return g.hasher.Hash([]byte(id))
}

func (g *AccessTokenGenerator) GenerateTrafficAccessToken(id api.SandboxID) (string, error) {
	key := fmt.Sprintf("%s-%s", sandboxTrafficPrefix, id)

	return g.hasher.Hash([]byte(key))
}
```

`packages/shared/pkg/keys/hmac_sha256.go` lines 1-25 (of 25)

```go
package keys

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

type HMACSha256Hashing struct {
	key []byte
}

func NewHMACSHA256Hashing(key []byte) *HMACSha256Hashing {
	return &HMACSha256Hashing{key: key}
}

func (h *HMACSha256Hashing) Hash(content []byte) (string, error) {
	mac := hmac.New(sha256.New, h.key)
	_, err := mac.Write(content)
	if err != nil {
		return "", err
	}

	return hex.EncodeToString(mac.Sum(nil)), nil
}
```

The envd token is only generated when the caller asked for `secure` (v2 create always does) and the
build's envd is >= 0.2.0; on resume/fork it is regenerated for the ID the sandbox will run under:

`packages/api/internal/handlers/sandbox_create.go` lines 641-677 (of 1038)

```go
func (a *APIStore) getEnvdAccessToken(envdVersion *string, sandboxID string) (string, *api.APIError) {
	if envdVersion == nil {
		return "", &api.APIError{
			Code:      http.StatusBadRequest,
			ClientMsg: "You need to re-build template to allow using secured access. Please visit https://e2b.dev/docs/sandbox/secured-access for more information.",
			Err:       errors.New("envd version is required during envd access token creation"),
		}
	}

	// check if the envd version is at least 0.2.0
	ok, err := sharedUtils.IsGTEVersion(*envdVersion, minEnvdVersionForSecureFlag)
	if err != nil {
		return "", &api.APIError{
			Code:      http.StatusInternalServerError,
			ClientMsg: "error during envd version check",
			Err:       err,
		}
	}
	if !ok {
		return "", &api.APIError{
			Code:      http.StatusBadRequest,
			ClientMsg: "Template is not compatible with secured access. Please visit https://e2b.dev/docs/sandbox/secured-access for more information.",
			Err:       errors.New("envd version is not supported for secure flag"),
		}
	}

	key, err := a.accessTokenGenerator.GenerateEnvdAccessToken(sandboxID)
	if err != nil {
		return "", &api.APIError{
			Code:      http.StatusInternalServerError,
			ClientMsg: "error during sandbox access token generation",
			Err:       err,
		}
	}

	return key, nil
}
```

### 3.2 Building `SandboxCreateRequest`: `packages/api/internal/orchestrator/create_instance.go`

Key field sources (commentary, all visible in the quote):
- `template_id` = template (env) ID; for a resume/fork it is the snapshot's env ID. `base_template_id` = the original template ID (for a fresh create both are the same).
- `build_id`, `kernel_version`, `firecracker_version`, `envd_version`, `vcpu`, `ram_mb`, `total_disk_size_mb` all come from the `env_builds` row (for snapshots: the snapshot's build row, which copies these from the running sandbox).
- `huge_pages` = `fcversion.New(build.FirecrackerVersion).HasHugePages()`.
- `snapshot` = `isResume` (true only for explicit/auto resume; **false for fork** even though build_id points at a snapshot build).
- `max_sandbox_length` = team limit in hours.
- `allow_internet_access=false` is translated into `network.egress.denied_cidrs = ["0.0.0.0/0"]` (the raw optional bool is also passed through).
- `network.ingress.traffic_access_token` is set only when public access is disabled.
- `start_time`/`end_time` = now and now+timeout.

`packages/api/internal/orchestrator/create_instance.go` lines 36-363 (of 682)

```go
// Semantic error codes for the explicit filesystem-boot (memory:false) path,
// emitted in APIError.ErrorCode so callers and metrics can distinguish these
// from ambient 4xx/5xx on the same routes.
const (
	ErrCodeStartInFlight             = "sandbox_start_in_flight"
	ErrCodeFilesystemBootUnconfirmed = "sandbox_filesystem_boot_unconfirmed"
)

// SandboxDataFetcher is a callback that fetches sandbox metadata.
// It is called after the concurrency lock is acquired to ensure fresh data.
type SandboxDataFetcher func(ctx context.Context) (SandboxMetadata, *api.APIError)

type SandboxMetadata struct {
	Metadata            map[string]string
	EnvVars             map[string]string
	Build               queries.EnvBuild
	AllowInternetAccess *bool
	Network             *types.SandboxNetworkConfig
	Alias               string
	TemplateID          string
	BaseTemplateID      string
	AutoPause           bool
	// AutoPauseFilesystemOnly makes a timeout auto-pause take a filesystem-only
	// snapshot instead of a full memory one. Only meaningful when AutoPause.
	AutoPauseFilesystemOnly bool
	AutoResume              *types.SandboxAutoResumeConfig
	VolumeMounts            []*orchestrator.SandboxVolumeMount
	EnvdAccessToken         *string
	// Iam records the sandbox workload identity configuration requested at create
	// time. Nil means workload identity is disabled.
	Iam    *types.SandboxIam
	NodeID *string
	// SnapshotSandboxID is the sandbox ID the resume snapshot is stored under.
	// It differs from the ID of the sandbox being started when forking.
	SnapshotSandboxID string
	// FilesystemBoot demands a cold boot of a memory-inclusive snapshot
	// (explicit memory:false resume). Set only by the resume data fetchers;
	// template creates and auto-resume never set it.
	FilesystemBoot bool
	// FilesystemOnlySnapshot marks a resume whose snapshot persists only the
	// rootfs. Unlike FilesystemBoot it describes the stored artifact rather than
	// the request, so every path that resumes such a snapshot sets it.
	FilesystemOnlySnapshot bool
}

// iamToProto maps the sandbox workload identity configuration into the
// orchestrator config. It returns nil when nothing is configured so older nodes
// and stored configs stay unchanged.
func iamToProto(iam *types.SandboxIam) *orchestrator.SandboxIam {
	if iam == nil || len(iam.Tokens) == 0 {
		return nil
	}

	protoTokens := make(map[string]*orchestrator.SandboxIamToken, len(iam.Tokens))
	for name, def := range iam.Tokens {
		protoTokens[name] = &orchestrator.SandboxIamToken{
			Audience:  def.Audience,
			TokenType: def.TokenType,
		}
	}

	return &orchestrator.SandboxIam{Tokens: protoTokens}
}

// buildEgressConfig constructs the orchestrator egress configuration from
// allow/deny entry lists. It splits allowed entries into CIDRs and domains,
// and adds the default nameserver when domains are present so the sandbox can
// resolve them.
func buildEgressConfig(allowedEntries, deniedEntries []string, rules map[string][]types.SandboxNetworkRule) *orchestrator.SandboxNetworkEgressConfig {
	allowedAddresses, allowedDomains := sandbox_network.ParseAddressesAndDomains(allowedEntries)

	if len(allowedDomains) > 0 {
		allowedAddresses = append(allowedAddresses, sandbox_network.DefaultNameserver)
	}

	var orchRules map[string]*orchestrator.SandboxNetworkDomainRules
	if rules != nil {
		orchRules = make(map[string]*orchestrator.SandboxNetworkDomainRules, len(rules))
		for domain, domainRules := range rules {
			orchRuleList := make([]*orchestrator.SandboxNetworkRule, 0, len(domainRules))
			for _, r := range domainRules {
				orchRule := &orchestrator.SandboxNetworkRule{}
				if r.Transform != nil {
					orchRule.Transform = &orchestrator.SandboxNetworkTransform{
						Headers: r.Transform.Headers,
					}
				}
				orchRuleList = append(orchRuleList, orchRule)
			}
			orchRules[domain] = &orchestrator.SandboxNetworkDomainRules{Rules: orchRuleList}
		}
	}

	return &orchestrator.SandboxNetworkEgressConfig{
		AllowedCidrs:   sandbox_network.AddressStringsToCIDRs(allowedAddresses),
		DeniedCidrs:    sandbox_network.AddressStringsToCIDRs(deniedEntries),
		AllowedDomains: allowedDomains,
		Rules:          orchRules,
	}
}

// applyEgressProxy copies BYOP SOCKS5 fields from src to dst. No-op on nil.
func applyEgressProxy(dst *orchestrator.SandboxNetworkEgressConfig, src *types.SandboxNetworkEgressConfig) {
	if dst == nil || src == nil {
		return
	}
	dst.EgressProxyAddress = src.EgressProxyAddress
	dst.EgressProxyUsername = src.EgressProxyUsername
	dst.EgressProxyPassword = src.EgressProxyPassword
}

// buildNetworkConfig constructs the orchestrator network configuration from the input parameters
func buildNetworkConfig(network *types.SandboxNetworkConfig, allowInternetAccess *bool, trafficAccessToken *string) *orchestrator.SandboxNetworkConfig {
	orchNetwork := &orchestrator.SandboxNetworkConfig{
		Egress: &orchestrator.SandboxNetworkEgressConfig{},
		Ingress: &orchestrator.SandboxNetworkIngressConfig{
			TrafficAccessToken: trafficAccessToken,
		},
	}

	if network != nil && network.Egress != nil {
		egress := buildEgressConfig(network.Egress.AllowedAddresses, network.Egress.DeniedAddresses, network.Egress.Rules)
		applyEgressProxy(egress, network.Egress)
		orchNetwork.Egress = egress
	}

	if network != nil && network.Ingress != nil {
		orchNetwork.Ingress.MaskRequestHost = network.Ingress.MaskRequestHost
		orchNetwork.Ingress.HttpsPorts = network.Ingress.HTTPSPorts
	}

	// Handle the case where internet access is explicitly disabled
	// This should be applied after copying the network config to preserve allowed addresses
	if allowInternetAccess != nil && !*allowInternetAccess {
		// Block all internet access - this overrides any other blocked addresses
		orchNetwork.Egress.DeniedCidrs = []string{sandbox_network.AllInternetTrafficCIDR}
	}

	return orchNetwork
}

func (o *Orchestrator) CreateSandbox(
	ctx context.Context,
	sandboxID,
	executionID string,
	team *teamtypes.Team,
	getSandboxData SandboxDataFetcher,
	startTime time.Time,
	endTime time.Time,
	timeout time.Duration,
	isResume bool,
	demandFilesystemBoot bool,
	creationMeta sandbox.CreationMetadata,
) (sbx sandbox.Sandbox, apiErr *api.APIError) {
	ctx, childSpan := tracer.Start(ctx, "create-sandbox")
	defer childSpan.End()

	// Calculate total concurrent instances including addons
	totalConcurrentInstances := team.Limits.SandboxConcurrency

	// Check if team has reached max instances
	finishStart, waitForStart, err := o.sandboxStore.Reserve(ctx, team.Team.ID, sandboxID, int(totalConcurrentInstances))
	if err != nil {
		var limitErr *sandbox.LimitExceededError

		switch {
		case errors.As(err, &limitErr):
			return sandbox.Sandbox{}, &api.APIError{
				Code: http.StatusTooManyRequests,
				ClientMsg: fmt.Sprintf(
					"you have reached the maximum number of concurrent E2B sandboxes (%d). If you need more, "+
						"please visit 'https://e2b.dev/docs/billing'", totalConcurrentInstances),
				Err: fmt.Errorf("team '%s' has reached the maximum number of instances (%d)", team.ID, totalConcurrentInstances),
			}
		default:
			logger.L().Error(ctx, "failed to reserve sandbox for team", logger.WithSandboxID(sandboxID), zap.Error(err))

			return sandbox.Sandbox{}, &api.APIError{
				Code:      http.StatusInternalServerError,
				ClientMsg: fmt.Sprintf("Failed to create sandbox: %s", err),
				Err:       err,
			}
		}
	}

	if waitForStart != nil {
		// A joined request rides whatever start is already in flight — which may
		// be a memory restore (e.g. a traffic-triggered auto-resume) that an
		// explicit memory:false must never be silently answered with.
		if demandFilesystemBoot {
			return sandbox.Sandbox{}, &api.APIError{
				Code:      http.StatusConflict,
				ErrorCode: ErrCodeStartInFlight,
				ClientMsg: "Sandbox is already starting; memory: false cannot be applied to a start already in flight — retry once it is running or paused",
				Err:       fmt.Errorf("filesystem-boot resume of '%s' cannot join an in-flight start", sandboxID),
			}
		}

		// Mark as a joined request for telemetry purposes
		joined.Mark(ctx)

		logger.L().Info(ctx, "sandbox is already being started, waiting for it to be ready", logger.WithSandboxID(sandboxID))

		sbx, err = waitForStart(ctx)
		if err != nil {
			logger.L().Warn(ctx, "Error waiting for sandbox to start", zap.Error(err), logger.WithSandboxID(sandboxID))

			if apiErr, ok := errors.AsType[*api.APIError](err); ok {
				return sandbox.Sandbox{}, apiErr
			}

			return sandbox.Sandbox{}, &api.APIError{
				Code:      http.StatusInternalServerError,
				ClientMsg: "Error waiting for sandbox to start",
				Err:       err,
			}
		}

		return sbx, nil
	}

	telemetry.ReportEvent(ctx, "Reserved sandbox for team")
	defer func() {
		// Don't change this handling
		// https://go.dev/play/p/4oy02s7BDMc
		if apiErr != nil {
			finishStart(sbx, apiErr)
		} else {
			finishStart(sbx, nil)
		}
	}()

	sbxData, fetchErr := getSandboxData(ctx)
	if fetchErr != nil {
		return sandbox.Sandbox{}, fetchErr
	}

	fcSemver, err := fcversion.New(sbxData.Build.FirecrackerVersion)
	if err != nil {
		errMsg := fmt.Errorf("failed to get fcSemver for firecracker fcSemver '%s': %w", sbxData.Build.FirecrackerVersion, err)

		return sandbox.Sandbox{}, &api.APIError{
			Code:      http.StatusInternalServerError,
			ClientMsg: "Failed to get build information for the template",
			Err:       errMsg,
		}
	}

	hasHugePages := fcSemver.HasHugePages()
	telemetry.ReportEvent(ctx, "Got FC info")

	var sbxDomain *string
	if team.ClusterID != nil {
		cluster, ok := o.clusters.GetClusterById(*team.ClusterID)
		if !ok {
			return sandbox.Sandbox{}, &api.APIError{
				Code:      http.StatusInternalServerError,
				ClientMsg: "Error while looking for sandbox cluster information",
				Err:       fmt.Errorf("cannot access cluster %s associated with team id %s that spawned sandbox %s", *team.ClusterID, team.ID, sandboxID),
			}
		}

		sbxDomain = cluster.SandboxDomain
	}

	var trafficAccessToken *string = nil
	network := sbxData.Network
	if network != nil && network.Ingress != nil && network.Ingress.AllowPublicAccess != nil && !*network.Ingress.AllowPublicAccess {
		accessToken, err := o.accessTokenGenerator.GenerateTrafficAccessToken(sandboxID)
		if err != nil {
			return sandbox.Sandbox{}, &api.APIError{
				Code:      http.StatusInternalServerError,
				ClientMsg: "Failed to create traffic access token",
				Err:       fmt.Errorf("failed to create traffic access token for sandbox %s: %w", sandboxID, err),
			}
		}

		trafficAccessToken = &accessToken
	}

	sbxNetwork := buildNetworkConfig(network, sbxData.AllowInternetAccess, trafficAccessToken)

	var orchAutoResume *orchestrator.SandboxAutoResumeConfig
	if sbxData.AutoResume != nil {
		orchAutoResume = &orchestrator.SandboxAutoResumeConfig{
			Policy:         string(sbxData.AutoResume.Policy),
			TimeoutSeconds: sbxData.AutoResume.Timeout,
		}
	}

	sbxRequest := &orchestrator.SandboxCreateRequest{
		Sandbox: &orchestrator.SandboxConfig{
			BaseTemplateId:          sbxData.BaseTemplateID,
			TemplateId:              sbxData.TemplateID,
			Alias:                   &sbxData.Alias,
			TeamId:                  team.ID.String(),
			BuildId:                 sbxData.Build.ID.String(),
			SandboxId:               sandboxID,
			ExecutionId:             executionID,
			KernelVersion:           sbxData.Build.KernelVersion,
			FirecrackerVersion:      sbxData.Build.FirecrackerVersion,
			EnvdVersion:             *sbxData.Build.EnvdVersion,
			Metadata:                sbxData.Metadata,
			EnvVars:                 sbxData.EnvVars,
			EnvdAccessToken:         sbxData.EnvdAccessToken,
			MaxSandboxLength:        team.Limits.MaxLengthHours,
			EventsTtlDays:           team.Limits.EventsTTLDays,
			HugePages:               hasHugePages,
			RamMb:                   sbxData.Build.RamMb,
			Vcpu:                    sbxData.Build.Vcpu,
			Snapshot:                isResume,
			AutoPause:               sbxData.AutoPause,
			AutoPauseFilesystemOnly: sbxData.AutoPauseFilesystemOnly,
			AutoResume:              orchAutoResume,
			AllowInternetAccess:     sbxData.AllowInternetAccess,
			Network:                 sbxNetwork,
			TotalDiskSizeMb:         ut.FromPtr(sbxData.Build.TotalDiskSizeMb),
			VolumeMounts:            sbxData.VolumeMounts,
			Iam:                     iamToProto(sbxData.Iam),
		},
		StartTime: timestamppb.New(startTime),
		EndTime:   timestamppb.New(endTime),
	}
	if sbxData.FilesystemBoot {
		// Left absent otherwise, so requests without the rescue are
		// byte-identical to before the field existed.
		sbxRequest.FilesystemBoot = new(true)
	}
```

`packages/api/internal/orchestrator/create_instance.go` lines 427-489 (of 682)

```go
	node = placed.Node

	// The sandbox was created successfully
	attributes := []attribute.KeyValue{
		attribute.Bool("is_resume", isResume),
		attribute.Bool("node_affinity_requested", affinityRequested),
		attribute.Bool("node_affinity_success", affinityRequested && node.ID == *sbxData.NodeID),
	}
	o.createdSandboxesCounter.Add(ctx, 1, metric.WithAttributes(attributes...))

	telemetry.SetAttributes(ctx, attribute.String("node.id", node.ID))
	telemetry.ReportEvent(ctx, "Created sandbox")

	// This is to compensate for the time it takes to start the instance
	// Otherwise it could cause the instance to expire before user has a chance to use it
	startTime = time.Now()
	endTime = startTime.Add(timeout)

	// The record carries the version the sandbox actually RUNS when the
	// orchestrator echoes it: the declared build version resolved through the
	// firecracker-versions flag at start and frozen for the sandbox's
	// lifetime. Version-gated paths branch on resolvedFCVersion — a resolved
	// version is exact, the declared fallback (old orchestrators) is only an
	// approximation of the running binary.
	recordFCVersion := placed.Response.GetResolvedFirecrackerVersion()
	resolvedFCVersion := recordFCVersion != ""
	if !resolvedFCVersion {
		recordFCVersion = sbxData.Build.FirecrackerVersion
	}

	sbx = sandbox.NewSandbox(
		sandboxID,
		sbxData.TemplateID,
		consts.ClientID,
		&sbxData.Alias,
		executionID,
		team.ID,
		sbxData.Build.ID,
		sbxData.Metadata,
		time.Duration(team.Limits.MaxLengthHours)*time.Hour,
		startTime,
		endTime,
		sbxData.Build.Vcpu,
		*sbxData.Build.TotalDiskSizeMb,
		sbxData.Build.RamMb,
		sbxData.Build.KernelVersion,
		recordFCVersion,
		*sbxData.Build.EnvdVersion,
		node.ID,
		node.ClusterID,
		sbxData.AutoPause,
		sbxData.AutoPauseFilesystemOnly,
		sbxData.AutoResume,
		sbxData.EnvdAccessToken,
		sbxData.AllowInternetAccess,
		sbxData.BaseTemplateID,
		sbxDomain,
		sbxData.Network,
		trafficAccessToken,
		nodemanager.ConvertOrchestratorMountsToDatabaseMounts(sbxData.VolumeMounts),
		sbxData.Iam,
	)
	sbx.FirecrackerVersionResolved = resolvedFCVersion
```

Constants referenced above:

`packages/shared/pkg/sandbox-network/firewall.go` lines 15-20 (of 175)

```go

const (
	AllInternetTrafficCIDR = "0.0.0.0/0"

	DefaultNameserver = "8.8.8.8"
)
```

`packages/shared/pkg/fcversion/version.go` lines 1-100 (of 118)

```go
package fcversion

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// ErrInvalidVersion reports a version string that is in none of the known
// formats. Callers must not fall back to guessing a format.
var ErrInvalidVersion = errors.New("invalid firecracker version")

// e2bVersionRe matches vX.Y-a.b.c: the upstream release line X.Y plus the
// e2b semver a.b.c, whose major is the snapshot/fork-API compatibility contract.
var e2bVersionRe = regexp.MustCompile(`^v(\d+\.\d+)-(\d+\.\d+\.\d+)$`)

type format int

const (
	formatUnknown format = iota // zero value: not a parsed version
	formatLegacy                // last_tag[-prerelease]_commit_hash, e.g. v1.14.1_431f1fc
	formatBare                  // upstream semver only, e.g. v1.10.1 (dev builds)
	formatE2B                   // vX.Y-<e2b-semver>, e.g. v1.14-0.1.0
)

type Info struct {
	format             format
	commitHash         string
	lastReleaseVersion semver.Version
	e2bVersion         semver.Version
}

func stripVersionPrefix(version string) string {
	return strings.TrimPrefix(version, "v")
}

func New(fcVersion string) (info Info, err error) {
	wrapInvalid := func(parseErr error) error {
		return fmt.Errorf("%w %q: %w", ErrInvalidVersion, fcVersion, parseErr)
	}

	if strings.Contains(fcVersion, "_") {
		parts := strings.Split(fcVersion, "_")

		version, versionErr := semver.StrictNewVersion(stripVersionPrefix(parts[0]))
		if versionErr != nil {
			return info, wrapInvalid(versionErr)
		}

		info.format = formatLegacy
		info.lastReleaseVersion = *version
		info.commitHash = parts[1]

		return info, nil
	}

	if m := e2bVersionRe.FindStringSubmatch(fcVersion); m != nil {
		line, versionErr := semver.StrictNewVersion(m[1] + ".0")
		if versionErr != nil {
			return info, wrapInvalid(versionErr)
		}

		e2bVersion, versionErr := semver.StrictNewVersion(m[2])
		if versionErr != nil {
			return info, wrapInvalid(versionErr)
		}

		info.format = formatE2B
		info.lastReleaseVersion = *line
		info.e2bVersion = *e2bVersion

		return info, nil
	}

	version, versionErr := semver.StrictNewVersion(stripVersionPrefix(fcVersion))
	if versionErr != nil {
		return info, wrapInvalid(versionErr)
	}

	info.format = formatBare
	info.lastReleaseVersion = *version

	return info, nil
}

// Version returns the upstream Firecracker version. For the e2b format only
// the release line is known, so patch is always 0.
func (v *Info) Version() semver.Version {
	return v.lastReleaseVersion
}

// E2BVersion returns the e2b release semver carried by e2b-format versions.
// ok is false for the other formats.
func (v *Info) E2BVersion() (e2bVersion semver.Version, ok bool) {
	if v.format != formatE2B {
		return semver.Version{}, false
	}
```

`packages/shared/pkg/fcversion/sandbox_features.go` lines 1-70 (of 70)

```go
package fcversion

import "github.com/Masterminds/semver/v3"

// Per-feature release floors. The gates are release-based on purpose: legacy
// (_hash) and bare dev builds never qualify even when a particular binary
// happens to carry the endpoints — the version string is the support
// contract, not a capability probe. The floors differ because the features
// shipped in different releases: fs-only snapshots never touch the balloon,
// so they must not inherit the in-place checkpoint's floor (0.1.x fleets run
// fs-only in production today).
var (
	// 0.2.0 introduced PATCH /balloon/reporting/{pause,resume} +
	// GET /balloon/reporting/status, which the in-place checkpoint's CoW
	// memory window drives while free-page reporting is live.
	inPlaceCheckpointMinE2B = semver.New(0, 2, 0, "", "")
	// Filesystem-only snapshots are part of the e2b release contract from
	// its first release.
	filesystemSnapshotsMinE2B = semver.New(0, 1, 0, "", "")
	// tscKhzTemplateMinE2B is the first release whose PUT /cpu-config accepts
	// x86_tsc_khz (v1.14-0.3.0). firecracker-versions must not map a line below
	// it: snapshots built above it store x86_tsc_khz, and their cold boots would fail.
	tscKhzTemplateMinE2B = semver.New(0, 3, 0, "", "")
)

func (v *Info) atLeastE2B(minVersion *semver.Version) bool {
	return v.format == formatE2B && !v.e2bVersion.LessThan(minVersion)
}

// HasInPlaceCheckpoint reports whether this build's release contract includes
// the in-place checkpoint (pause, snapshot, resume the same FC process, with
// the deferred CoW memory export). Callers fall back to the resume-fresh
// checkpoint when false.
func (v *Info) HasInPlaceCheckpoint() bool {
	return v.atLeastE2B(inPlaceCheckpointMinE2B)
}

// HasFilesystemSnapshots reports whether this build's release contract
// includes producing filesystem-only (memoryless) snapshots. Callers refuse
// the request when false — silently taking a memory snapshot instead would
// betray an explicit memory:false.
func (v *Info) HasFilesystemSnapshots() bool {
	return v.atLeastE2B(filesystemSnapshotsMinE2B)
}

// HasTscKhzTemplate reports whether this build's custom CPU template accepts
// x86_tsc_khz.
func (v *Info) HasTscKhzTemplate() bool {
	return v.atLeastE2B(tscKhzTemplateMinE2B)
}

func (v *Info) HasHugePages() bool {
	if v.lastReleaseVersion.Major() > 1 || (v.lastReleaseVersion.Major() == 1 && v.lastReleaseVersion.Minor() >= 7) {
		return true
	}

	return false
}

func (v *Info) HasFreePageReporting() bool {
	return v.lastReleaseVersion.Major() > 1 || (v.lastReleaseVersion.Major() == 1 && v.lastReleaseVersion.Minor() >= 14)
}

func (v *Info) HasFreePageHinting() bool {
	return v.lastReleaseVersion.Major() > 1 || (v.lastReleaseVersion.Major() == 1 && v.lastReleaseVersion.Minor() >= 14)
}

func (v *Info) HasMemfd() bool {
	return v.lastReleaseVersion.Major() > 1 || (v.lastReleaseVersion.Major() == 1 && v.lastReleaseVersion.Minor() >= 14)
}
```

`executionID` and the call into CreateSandbox (`packages/api/internal/handlers/sandbox.go`):

`packages/api/internal/handlers/sandbox.go` lines 52-102 (of 124)

```go
// startSandboxInternal starts the sandbox and returns the internal sandbox model (includes routing info).
func (a *APIStore) startSandboxInternal(
	ctx context.Context,
	sandboxID string,
	timeout time.Duration,
	team *typesteam.Team,
	getSandboxData orchestrator.SandboxDataFetcher,
	requestHeader *http.Header,
	isResume bool,
	demandFilesystemBoot bool,
	mcp api.Mcp,
) (sandbox.Sandbox, *api.APIError) {
	startTime := time.Now()
	endTime := startTime.Add(timeout)

	// Unique ID for the execution (from start/resume to stop/pause)
	executionID := uuid.New().String()

	creationMeta := buildCreationMetadata(team, requestHeader, isResume, mcp)

	sbx, instanceErr := a.orchestrator.CreateSandbox(
		ctx,
		sandboxID,
		executionID,
		team,
		getSandboxData,
		startTime,
		endTime,
		timeout,
		isResume,
		demandFilesystemBoot,
		creationMeta,
	)
	if instanceErr != nil {
		telemetry.ReportError(ctx, "error when creating instance", instanceErr.Err)

		return sandbox.Sandbox{}, instanceErr
	}

	telemetry.ReportEvent(ctx, "Created sandbox")

	go func() {
		a.templateSpawnCounter.IncreaseTemplateSpawnCount(sbx.BaseTemplateID, time.Now())
	}()

	telemetry.SetAttributes(ctx,
		attribute.String("instance.id", sbx.SandboxID),
	)

	return sbx, nil
}
```

The fresh-create handler (sandbox ID generation, defaults, secure flag, allow_internet_access, network,
the data fetcher that feeds §3.2). Default timeouts: v1 create 15 s, v2 create 5 min; `autoPause` default false.

`packages/api/internal/handlers/sandbox_create.go` lines 47-64 (of 1038)

```go
const (
	InstanceIDPrefix            = "i"
	metricTemplateAlias         = metrics.MetricPrefix + "template.alias"
	metricMemoryOverride        = metrics.MetricPrefix + "memory_override"
	minEnvdVersionForSecureFlag = "0.2.0" // Minimum version of envd that supports secure flag

	// Network validation error messages
	ErrMsgDomainsRequireBlockAll = "When specifying allowed domains in allow out, you must include 'ALL_TRAFFIC' in deny out to block all other traffic."

	maxHTTPSPorts                     = 128
	maxNetworkRuleTransformsPerDomain = 1
	maxNetworkRuleDomainLen           = 128
	maxNetworkRuleHeaderNameLen       = 64
	maxNetworkRuleHeaderValueLen      = 2048
	maxNetworkRuleHeadersPerRule      = 20

	maxIamTokens = 5
)
```

`packages/api/internal/handlers/sandbox_create.go` lines 97-413 (of 1038)

```go
func newSandboxFromV2(body api.NewSandboxV2) api.NewSandbox {
	secure := true

	return api.NewSandbox{
		TemplateID:          body.TemplateID,
		Timeout:             body.Timeout,
		AutoPause:           body.AutoPause,
		AutoPauseMemory:     body.AutoPauseMemory,
		AutoResume:          body.AutoResume,
		Secure:              &secure,
		AllowInternetAccess: body.AllowInternetAccess,
		Network:             body.Network,
		Metadata:            body.Metadata,
		EnvVars:             body.EnvVars,
		Mcp:                 body.Mcp,
		Iam:                 body.Iam,
		VolumeMounts:        body.VolumeMounts,
	}
}

// createSandbox runs the shared create flow; defaultTimeout applies when the body omits timeout.
func (a *APIStore) createSandbox(c *gin.Context, body api.NewSandbox, defaultTimeout time.Duration) {
	ctx := c.Request.Context()

	// Get team from context, use TeamContextKey
	teamInfo := auth.MustGetTeamInfo(c)

	c.Set("teamID", teamInfo.Team.ID.String())

	span := trace.SpanFromContext(ctx)
	traceID := span.SpanContext().TraceID().String()
	c.Set("traceID", traceID)

	telemetry.ReportEvent(ctx, "Parsed body")

	identifier, tag, err := id.ParseName(body.TemplateID)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Invalid template reference: %s", err))
		telemetry.ReportError(ctx, "invalid template reference", err)

		return
	}

	clusterID := clusters.WithClusterFallback(teamInfo.Team.ClusterID)
	aliasInfo, err := a.templateCache.ResolveAlias(ctx, identifier, teamInfo.Team.Slug)
	if err != nil {
		apiErr := templatecache.ErrorToAPIError(err, identifier)
		telemetry.ReportErrorByCode(ctx, apiErr.Code, "error when resolving template alias", apiErr.Err, attribute.String("identifier", identifier))
		a.sendAPIStoreError(c, apiErr.Code, apiErr.ClientMsg)

		return
	}

	env, build, err := a.templateCache.Get(ctx, aliasInfo.TemplateID, tag, teamInfo.Team.ID, clusterID)
	if err != nil {
		visible := aliasInfo.TeamID == teamInfo.Team.ID
		if metadata, mErr := a.templateCache.GetMetadata(ctx, aliasInfo.TemplateID); mErr == nil {
			visible = visible || metadata.Public
		}

		ref := templatecache.TemplateRef{
			Identifier: aliasInfo.MatchedIdentifier,
			Visible:    visible,
		}

		apiErr := ref.APIError(err)
		telemetry.ReportErrorByCode(ctx, apiErr.Code, "error when getting template", apiErr.Err, telemetry.WithTemplateID(aliasInfo.TemplateID))
		a.sendAPIStoreError(c, apiErr.Code, apiErr.ClientMsg)

		return
	}

	telemetry.ReportEvent(ctx, "Checked team access")

	c.Set("envID", env.TemplateID)
	setTemplateNameMetric(ctx, c, a.featureFlags, env.TemplateID, env.Names)

	sandboxID := InstanceIDPrefix + id.Generate()

	c.Set("instanceID", sandboxID)

	sbxlogger.E(&sbxlogger.SandboxMetadata{
		SandboxID:  sandboxID,
		TemplateID: env.TemplateID,
		TeamID:     teamInfo.Team.ID.String(),
	}).Debug(ctx, "Started creating sandbox")

	alias := firstAlias(env.Aliases)
	telemetry.SetAttributes(ctx,
		telemetry.WithSandboxID(sandboxID),
		telemetry.WithTemplateID(env.TemplateID),
		telemetry.WithBuildID(build.ID.String()),
		attribute.String("env.alias", alias),
		telemetry.WithKernelVersion(build.KernelVersion),
		telemetry.WithFirecrackerVersion(build.FirecrackerVersion),
	)

	autoPause := sharedUtils.DerefOrDefault(body.AutoPause, sandbox.AutoPauseDefault)
	// autoPauseMemory defaults to true (full memory snapshot). When false, a
	// timeout auto-pause takes a filesystem-only snapshot (cold-boots on resume).
	autoPauseFilesystemOnly := !sharedUtils.DerefOrDefault(body.AutoPauseMemory, true)
	envVars := sharedUtils.DerefOrDefault(body.EnvVars, nil)
	mcp := sharedUtils.DerefOrDefault(body.Mcp, nil)
	metadata := sharedUtils.DerefOrDefault(body.Metadata, nil)
	apiVolumeMounts := sharedUtils.DerefOrDefault(body.VolumeMounts, nil)

	timeout, apiErr := validateAndParseTimeoutWithDefault(body.Timeout, teamInfo.Limits.MaxLengthHours, defaultTimeout)
	if apiErr != nil {
		a.sendAPIStoreError(c, apiErr.Code, apiErr.ClientMsg)

		return
	}

	autoResume := buildAutoResumeConfig(body.AutoResume)
	if autoResume != nil {
		minAutoResumeTimeout := time.Duration(a.featureFlags.IntFlag(ctx, featureflags.MinAutoResumeTimeoutSeconds)) * time.Second
		autoResume.Timeout = calculateTimeoutSeconds(timeout, minAutoResumeTimeout, teamInfo)
	}

	// autoPauseMemory only controls the snapshot kind of a timeout auto-pause, so
	// it is meaningless without autoPause; reject it rather than silently storing
	// a no-op policy.
	if autoPauseFilesystemOnly && !autoPause {
		a.sendAPIStoreError(c, http.StatusBadRequest, "autoPauseMemory=false only applies when autoPause is true.")

		return
	}

	// A filesystem-only auto-pause produces a snapshot that traffic cannot
	// auto-resume (it must be resumed explicitly), so the two are incompatible.
	if autoPauseFilesystemOnly && autoResume != nil && autoResume.Policy == types.SandboxAutoResumeAny {
		a.sendAPIStoreError(c, http.StatusBadRequest, "autoPauseMemory=false (filesystem-only auto-pause) cannot be combined with autoResume: a filesystem-only snapshot cannot be auto-resumed by traffic and must be resumed explicitly.")

		return
	}

	// A filesystem-only auto-pause also needs the sandbox's Firecracker
	// release to carry the feature. No record exists yet, so fcgate checks
	// the declared build version and falls back to the same flag resolution
	// the orchestrator will apply at start — refusing up front instead of
	// storing a policy the timeout eviction would have to degrade later.
	// (The evictor still degrades gracefully if resolution shifts between
	// create and timeout.)
	if autoPauseFilesystemOnly && !fcgate.SupportsFilesystemSnapshotsDeclared(ctx, a.featureFlags, build.FirecrackerVersion) {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("autoPauseMemory=false requires a Firecracker release with filesystem-only snapshot support; this template's version is %q. Rebuild the template on a current release or omit autoPauseMemory.", build.FirecrackerVersion))

		return
	}

	var envdAccessToken *string = nil
	if body.Secure != nil && *body.Secure == true {
		accessToken, tokenErr := a.getEnvdAccessToken(build.EnvdVersion, sandboxID)
		if tokenErr != nil {
			telemetry.ReportError(ctx, "secure envd access token error", tokenErr.Err, telemetry.WithSandboxID(sandboxID), telemetry.WithBuildID(build.ID.String()))
			a.sendAPIStoreError(c, tokenErr.Code, tokenErr.ClientMsg)

			return
		}

		envdAccessToken = &accessToken
	}

	iamCfg, iamErr := buildSandboxIam(body.Iam)
	if iamErr != nil {
		telemetry.ReportError(ctx, "invalid iam config", iamErr.Err, telemetry.WithSandboxID(sandboxID))
		a.sendAPIStoreError(c, iamErr.Code, iamErr.ClientMsg)

		return
	}

	if iamCfg != nil && !a.featureFlags.BoolFlag(ctx, featureflags.SandboxIamTokensFlag, featureflags.TeamContext(teamInfo.Team.ID.String())) {
		a.sendAPIStoreError(c, http.StatusBadRequest, "Sandbox IAM workload tokens are not available for your team.")

		return
	}

	allowInternetAccess := body.AllowInternetAccess

	var network *types.SandboxNetworkConfig
	if n := body.Network; n != nil {
		maxDomains := a.featureFlags.IntFlag(ctx, featureflags.MaxNetworkRuleDomains, featureflags.TeamContext(teamInfo.Team.ID.String()))
		if err := validateNetworkConfig(ctx, a.featureFlags, teamInfo.Team.ID, sharedUtils.DerefOrDefault(build.EnvdVersion, ""), maxDomains, n); err != nil {
			telemetry.ReportError(ctx, "invalid network config", err.Err, telemetry.WithSandboxID(sandboxID))
			a.sendAPIStoreError(c, err.Code, err.ClientMsg)

			return
		}

		network = &types.SandboxNetworkConfig{
			Ingress: &types.SandboxNetworkIngressConfig{
				AllowPublicAccess: n.AllowPublicTraffic,
				MaskRequestHost:   n.MaskRequestHost,
				HTTPSPorts:        sharedUtils.DerefOrDefault(n.HttpsPorts, nil),
			},
			Egress: &types.SandboxNetworkEgressConfig{
				AllowedAddresses: sharedUtils.DerefOrDefault(n.AllowOut, nil),
				DeniedAddresses:  sharedUtils.DerefOrDefault(n.DenyOut, nil),
				Rules:            apiRulesToDBRules(n.Rules),
			},
		}

		if ep := n.EgressProxy; ep != nil {
			if !a.featureFlags.BoolFlag(ctx, featureflags.BYOPProxyEnabledFlag) {
				telemetry.ReportEvent(ctx, "egressProxy rejected by BYOPProxyEnabledFlag")
				a.sendAPIStoreError(c, http.StatusForbidden,
					"Egress proxy (network.egressProxy) is not enabled for this team.")

				return
			}

			canonical, err := sandbox_network.ValidateEgressProxy(ctx, &sandbox_network.EgressProxyConfig{
				Address:  ep.Address,
				Username: sharedUtils.DerefOrDefault(ep.Username, ""),
				Password: sharedUtils.DerefOrDefault(ep.Password, ""),
			}, nil)
			if err != nil {
				telemetry.ReportError(ctx, "invalid egress proxy config", err, telemetry.WithSandboxID(sandboxID))
				a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Invalid egress proxy config: %s", err))

				return
			}

			network.Egress.EgressProxyAddress = canonical.Address
			network.Egress.EgressProxyUsername = canonical.Username
			network.Egress.EgressProxyPassword = canonical.Password
		}

		// Make sure envd seucre access is enforced when public access is disabled,
		// This requirement forces users using newer features to secure sandboxes properly.
		if !sharedUtils.DerefOrDefault(network.Ingress.AllowPublicAccess, types.AllowPublicAccessDefault) && envdAccessToken == nil {
			a.sendAPIStoreError(c, http.StatusBadRequest, "You cannot create a sandbox without public access unless you enable secure envd access via 'secure' flag.")

			return
		}
	}

	sbxVolumeMounts, err := convertAPIVolumesToOrchestratorVolumes(
		ctx, a.sqlcDB, a.featureFlags, teamInfo.ID, apiVolumeMounts, build,
	)
	if err != nil {
		if errors.Is(err, errVolumesNotSupported) || errors.Is(err, errNoEnvdVersion) {
			a.sendAPIStoreError(c, http.StatusBadRequest, err.Error())

			return
		}

		if errors.Is(err, ErrVolumeMountsDisabled) {
			a.sendAPIStoreError(c, http.StatusBadRequest, "Volume mounts are not enabled.")

			return
		}

		if vne, ok := errors.AsType[InvalidVolumeMountsError](err); ok {
			a.sendAPIStoreError(c, http.StatusBadRequest, vne.Error())

			return
		}

		telemetry.ReportError(ctx, "failed to convert volume mounts", err, telemetry.WithSandboxID(sandboxID))
		a.sendAPIStoreError(c, http.StatusInternalServerError, "failed to convert volume mounts")

		return
	}

	getSandboxData := func(_ context.Context) (apiorch.SandboxMetadata, *api.APIError) {
		// The data can't be influenced by action on the same sandbox as other operations,
		// so it's safe to reuse the data
		return apiorch.SandboxMetadata{
			Metadata:                metadata,
			EnvVars:                 envVars,
			Build:                   *build,
			AllowInternetAccess:     allowInternetAccess,
			Network:                 network,
			Alias:                   alias,
			TemplateID:              env.TemplateID,
			BaseTemplateID:          env.TemplateID,
			AutoPause:               autoPause,
			AutoPauseFilesystemOnly: autoPauseFilesystemOnly,
			AutoResume:              autoResume,
			VolumeMounts:            sbxVolumeMounts,
			EnvdAccessToken:         envdAccessToken,
			Iam:                     iamCfg,
		}, nil
	}

	sbx, createErr := a.startSandbox(
		ctx,
		sandboxID,
		timeout,
		teamInfo,
		getSandboxData,
		&c.Request.Header,
		false,
		false,
		mcp,
	)
	if createErr != nil {
		apierrors.SendAPIError(c, createErr)

		return
	}

	if n := body.Network; n != nil && n.Rules != nil && len(*n.Rules) > 0 {
		domains := make([]string, 0, len(*n.Rules))
		for domain := range *n.Rules {
			domains = append(domains, domain)
		}

		a.posthog.CreateAnalyticsTeamEvent(ctx, teamInfo.Team.ID.String(), "sandbox with network transform rules created",
			a.posthog.GetPackageToPosthogProperties(&c.Request.Header).
				Set("sandbox_id", sandboxID).
				Set("domains", domains),
		)
	}

	c.JSON(http.StatusCreated, &sbx)
}
```

`packages/api/internal/sandbox/sandboxtypes/states.go` lines 100-110 (of 118)

```go
	StateSnapshotting: {StateRunning: true, StateKilling: true, StatePausing: true},
}

const (
	SandboxTimeoutDefault = time.Second * 15
	// Timeout applied by the v2 create and connect endpoints when the request omits one
	SandboxTimeoutDefaultV2 = time.Minute * 5
	// Should we auto pause the instance by default instead of killing it
	AutoPauseDefault = false
)

```

`packages/api/internal/handlers/timeout_helper.go` lines 18-43 (of 76)

```go
func validateAndParseTimeout(rawTimeout *int32, maxHours int64) (time.Duration, *api.APIError) {
	return validateAndParseTimeoutWithDefault(rawTimeout, maxHours, sandbox.SandboxTimeoutDefault)
}

func validateAndParseTimeoutWithDefault(rawTimeout *int32, maxHours int64, fallback time.Duration) (time.Duration, *api.APIError) {
	timeout := fallback
	if rawTimeout != nil {
		if *rawTimeout <= 0 {
			return 0, &api.APIError{
				Code:      http.StatusBadRequest,
				ClientMsg: "Timeout must be greater than 0",
			}
		}

		timeout = time.Duration(*rawTimeout) * time.Second

		if maxDuration := time.Duration(maxHours) * time.Hour; timeout > maxDuration {
			return 0, &api.APIError{
				Code:      http.StatusBadRequest,
				ClientMsg: fmt.Sprintf("Timeout cannot be greater than %d hours", maxHours),
			}
		}
	}

	return timeout, nil
}
```

### 3.3 Fork: `packages/api/internal/handlers/sandbox_fork.go` (complete)

Fork = Checkpoint the running original in place (§3.5), then N fresh `CreateSandbox` calls with new
sandbox IDs, `isResume=false`, whose data comes from the original's snapshot row
(`buildResumeSandboxDataFromSnapshot(originalID, newID, ...)`).

`packages/api/internal/handlers/sandbox_fork.go` lines 1-239 (of 239)

```go
package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator"
	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	"github.com/e2b-dev/infra/packages/api/internal/utils"
	"github.com/e2b-dev/infra/packages/auth/pkg/auth"
	"github.com/e2b-dev/infra/packages/shared/pkg/ginutils"
	"github.com/e2b-dev/infra/packages/shared/pkg/id"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	sbxlogger "github.com/e2b-dev/infra/packages/shared/pkg/logger/sandbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	sharedUtils "github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// maxForkCount caps how many sandboxes a single fork request can create,
// bounding the parallel boots (and the result allocation) per request.
const maxForkCount = 100

// PostSandboxesSandboxIDFork forks a running sandbox: it checkpoints the
// sandbox in place (snapshot it and resume it on its node, so the original
// keeps running with its ID and expiration untouched) and creates count new
// sandboxes from that snapshot under fresh IDs. A fork starts as a new sandbox,
// not a resume, so placement spreads the forks across the cluster instead of
// pinning every one to the original's node. Each fork succeeds or fails
// independently: the response carries one result per requested fork, holding
// either the created sandbox or the error that prevented it from starting.
func (a *APIStore) PostSandboxesSandboxIDFork(c *gin.Context, sandboxID api.SandboxID) {
	ctx := c.Request.Context()

	teamInfo := auth.MustGetTeamInfo(c)
	teamID := teamInfo.Team.ID

	sandboxID, err := utils.ShortID(sandboxID)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, "Invalid sandbox ID")

		return
	}

	span := trace.SpanFromContext(ctx)
	span.SetAttributes(telemetry.WithSandboxID(sandboxID))

	traceID := span.SpanContext().TraceID().String()
	c.Set("traceID", traceID)

	body, err := ginutils.ParseOptionalBody[api.PostSandboxesSandboxIDForkJSONRequestBody](ctx, c)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Error when parsing request: %s", err))

		return
	}

	forkTimeout, apiErr := validateAndParseTimeout(body.Timeout, teamInfo.Limits.MaxLengthHours)
	if apiErr != nil {
		a.sendAPIStoreError(c, apiErr.Code, apiErr.ClientMsg)

		return
	}

	forkCount := 1
	if body.Count != nil {
		forkCount = int(*body.Count)
	}

	if forkCount < 1 {
		a.sendAPIStoreError(c, http.StatusBadRequest, "Count must be at least 1")

		return
	}

	if forkCount > maxForkCount {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Count cannot be greater than %d", maxForkCount))

		return
	}

	// The original sandbox keeps running and holds one slot, so more forks
	// than the concurrency limit can never succeed.
	if int64(forkCount) >= teamInfo.Limits.SandboxConcurrency {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Count must be lower than the maximum number of concurrent sandboxes (%d)", teamInfo.Limits.SandboxConcurrency))

		return
	}

	original, err := a.orchestrator.GetSandbox(ctx, teamID, sandboxID)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			apiErr := forkHandleNotRunningSandbox(ctx, a, sandboxID, teamID)
			a.sendAPIStoreError(c, apiErr.Code, apiErr.ClientMsg)

			return
		}

		telemetry.ReportError(ctx, "error getting sandbox for fork", err, telemetry.WithSandboxID(sandboxID))
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error forking sandbox")

		return
	}

	if err := sharedUtils.CheckEnvdVersionForSnapshot(original.EnvdVersion); err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, err.Error())

		return
	}

	// Checkpoint the sandbox in place: it is briefly paused on its node,
	// snapshotted, and resumed under the same execution ID, so the original
	// keeps its ID, expiration, and concurrency slot.
	err = a.orchestrator.CheckpointSandbox(ctx, teamID, sandboxID)
	var transErr *sandbox.InvalidStateTransitionError

	switch {
	case err == nil:
	case errors.Is(err, sandbox.ErrNotFound):
		apiErr := forkHandleNotRunningSandbox(ctx, a, sandboxID, teamID)
		a.sendAPIStoreError(c, apiErr.Code, apiErr.ClientMsg)

		return
	case errors.As(err, &transErr):
		a.sendAPIStoreError(c, http.StatusConflict, fmt.Sprintf("Sandbox '%s' cannot be forked while in '%s' state", sandboxID, transErr.CurrentState))

		return
	case errors.Is(err, orchestrator.PauseQueueExhaustedError{}):
		a.sendAPIStoreError(c, http.StatusServiceUnavailable, fmt.Sprintf("Sandbox '%s' cannot be forked right now because its node is busy, please retry", sandboxID))

		return
	default:
		telemetry.ReportError(ctx, "error checkpointing sandbox for fork", err, telemetry.WithSandboxID(sandboxID))
		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error forking sandbox")

		return
	}

	sbxlogger.E(&sbxlogger.SandboxMetadata{
		SandboxID:  sandboxID,
		TemplateID: original.TemplateID,
		TeamID:     teamID.String(),
	}).Debug(ctx, "Creating forked sandboxes from snapshot", zap.Int("count", forkCount))

	// All forks boot in parallel from the same immutable snapshot, each
	// succeeding or failing independently.
	results := make([]api.SandboxForkResult, forkCount)
	forkNodeIDs := make([]string, forkCount)

	wg := errgroup.Group{}
	for i := range forkCount {
		wg.Go(func() error {
			forkedSandboxID := InstanceIDPrefix + id.Generate()

			forkedSbx, createErr := a.startSandboxInternal(
				ctx,
				forkedSandboxID,
				forkTimeout,
				teamInfo,
				a.buildResumeSandboxDataFromSnapshot(sandboxID, forkedSandboxID, nil, nil),
				&c.Request.Header,
				false, // isResume
				false,
				nil, // mcp
			)
			if createErr != nil {
				telemetry.ReportError(ctx, "error creating forked sandbox", createErr.Err, telemetry.WithSandboxID(forkedSandboxID))
				results[i] = api.SandboxForkResult{Error: &api.Error{Code: int32(createErr.Code), Message: createErr.ClientMsg}}

				//nolint:nilerr // per-fork errors are reported in the result entry, not propagated
				return nil
			}

			results[i] = api.SandboxForkResult{Sandbox: forkedSbx.ToAPISandbox()}
			forkNodeIDs[i] = forkedSbx.NodeID

			return nil
		})
	}
	_ = wg.Wait()

	started, nodes := forkOutcome(results, forkNodeIDs)
	telemetry.SetAttributes(ctx,
		attribute.Int("fork.count", forkCount),
		attribute.Int("fork.started", started),
		attribute.Int("fork.failed", forkCount-started),
		attribute.Int("fork.nodes", nodes),
	)
	logger.L().Info(ctx, "Forked sandbox",
		logger.WithSandboxID(sandboxID),
		logger.WithTeamID(teamID.String()),
		logger.WithTemplateID(original.TemplateID),
		zap.Int("fork_count", forkCount),
		zap.Int("fork_started", started),
		zap.Int("fork_failed", forkCount-started),
		zap.Int("fork_nodes", nodes),
	)

	c.JSON(http.StatusCreated, results)
}

// forkOutcome counts the forks that started and the distinct nodes they started
// on; nodeIDs is indexed like results.
func forkOutcome(results []api.SandboxForkResult, nodeIDs []string) (started, nodes int) {
	seen := make(map[string]struct{}, len(nodeIDs))
	for i, result := range results {
		if result.Sandbox == nil {
			continue
		}

		started++
		seen[nodeIDs[i]] = struct{}{}
	}

	return started, len(seen)
}

// forkHandleNotRunningSandbox classifies a fork request for a sandbox that is
// not running: 409 if it is paused (a snapshot exists), 404 otherwise.
func forkHandleNotRunningSandbox(ctx context.Context, a *APIStore, sandboxID string, teamID uuid.UUID) api.APIError {
	apiErr := pauseHandleNotRunningSandbox(ctx, a.snapshotCache, sandboxID, teamID)
	switch apiErr.Code {
	case http.StatusConflict:
		apiErr.ClientMsg = fmt.Sprintf("Sandbox '%s' is paused and cannot be forked; resume it first", sandboxID)
	case http.StatusInternalServerError:
		apiErr.ClientMsg = "Error forking sandbox"
	}

	return apiErr
}
```

### 3.4 Resume: data fetcher and handler call site (`packages/api/internal/handlers/sandbox_resume.go`)

Resume = `CreateSandbox(..., isResume=true)` with the same sandbox ID, template_id/build_id from the
latest snapshot row, node affinity to the node that took the snapshot.

`packages/api/internal/handlers/sandbox_resume.go` lines 47-90 (of 397)

```go
func (a *APIStore) PostSandboxesSandboxIDResume(c *gin.Context, sandboxID api.SandboxID) {
	ctx := c.Request.Context()

	// Get team from context, use TeamContextKey
	teamInfo := auth.MustGetTeamInfo(c)

	span := trace.SpanFromContext(ctx)
	traceID := span.SpanContext().TraceID().String()
	c.Set("traceID", traceID)

	sandboxID, err := utils.ShortID(sandboxID)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, "Invalid sandbox ID")

		return
	}

	span.SetAttributes(telemetry.WithSandboxID(sandboxID))

	// The body is optional: every field defaults, so tolerate an absent one.
	body, err := ginutils.ParseOptionalBody[api.PostSandboxesSandboxIDResumeJSONRequestBody](ctx, c)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Error when parsing request: %s", err))

		telemetry.ReportCriticalError(ctx, "error when parsing request", err)

		return
	}

	telemetry.ReportEvent(ctx, "Parsed body")

	timeout, apiErr := validateAndParseTimeout(body.Timeout, teamInfo.Limits.MaxLengthHours)
	if apiErr != nil {
		a.sendAPIStoreError(c, apiErr.Code, apiErr.ClientMsg)

		return
	}

	teamID := teamInfo.Team.ID
	backend := a.resumeBackend()

	sandboxData, err := backend.GetSandbox(ctx, teamID, sandboxID)
	if err == nil {
		if sandboxData.TeamID != teamID {
```

`packages/api/internal/handlers/sandbox_resume.go` lines 180-213 (of 397)

```go
	// 400 even when the start would otherwise join an in-flight one (409).
	if _, apiErr := resolveFilesystemBoot(ctx, a.featureFlags, body.Memory, lastSnapshot.Snapshot); apiErr != nil {
		setMemoryOverrideOutcome(c, body.Memory, apiErr)
		apierrors.SendAPIError(c, apiErr)

		return
	}

	sbxlogger.E(&sbxlogger.SandboxMetadata{
		SandboxID:  sandboxID,
		TemplateID: lastSnapshot.Snapshot.EnvID,
		TeamID:     teamID.String(),
	}).Debug(ctx, "Started resuming sandbox")

	sbx, createErr := a.startSandbox(
		ctx,
		sandboxID,
		timeout,
		teamInfo,
		a.buildResumeSandboxData(sandboxID, body.AutoPause, body.Memory),
		&c.Request.Header,
		true,
		demandsFilesystemBoot(body.Memory, lastSnapshot.Snapshot),
		nil, // mcp
	)
	setMemoryOverrideOutcome(c, body.Memory, createErr)
	if createErr != nil {
		apierrors.SendAPIError(c, createErr)

		return
	}

	c.JSON(http.StatusCreated, &sbx)
}
```

`packages/api/internal/handlers/sandbox_resume.go` lines 230-397 (of 397)

```go
// buildResumeSandboxData returns a SandboxDataFetcher for resuming a sandbox
// from its own snapshot. memory is the request's optional memory field; nil
// (the implicit paths: auto-resume, fork) means a plain resume.
func (a *APIStore) buildResumeSandboxData(sandboxID string, autoPauseOverride, memory *bool) orchestrator.SandboxDataFetcher {
	return a.buildResumeSandboxDataFromSnapshot(sandboxID, sandboxID, autoPauseOverride, memory)
}

const errCodeMemoryOverrideDisabled = "sandbox_memory_override_disabled"

// setMemoryOverrideOutcome labels the request metric with the fate of an
// explicit memory:false so the ramp is measurable on http.server.duration:
// served, or rejected (flag off, join refused, unconfirmed echo, other).
func setMemoryOverrideOutcome(c *gin.Context, memory *bool, createErr *api.APIError) {
	if memory == nil || *memory {
		return
	}

	outcome := "served"
	switch {
	case createErr == nil:
	case createErr.ErrorCode == errCodeMemoryOverrideDisabled:
		outcome = "rejected_flag_off"
	case createErr.ErrorCode == orchestrator.ErrCodeStartInFlight:
		outcome = "rejected_in_flight_start"
	case createErr.ErrorCode == orchestrator.ErrCodeFilesystemBootUnconfirmed:
		outcome = "rejected_unconfirmed"
	default:
		outcome = "error"
	}
	c.Set(metricMemoryOverride, outcome)
}

// snapshotIsFilesystemOnly reports whether the stored snapshot persists only
// the rootfs. Rows written before the kind was recorded have a nil config and
// are memory snapshots.
func snapshotIsFilesystemOnly(snap queries.Snapshot) bool {
	return snap.Config != nil && snap.Config.FilesystemOnly
}

// demandsFilesystemBoot reports whether the request explicitly demands a cold
// boot that an in-flight start might not honor: memory:false on a snapshot not
// already filesystem-only (an fs-only snapshot cold-boots on any start, so a
// join is safe for it).
func demandsFilesystemBoot(memory *bool, snap queries.Snapshot) bool {
	if memory == nil || *memory {
		return false
	}

	return !snapshotIsFilesystemOnly(snap)
}

// resolveFilesystemBoot maps the request's optional memory field (default
// true) to the create RPC's filesystem-boot demand. Flag off rejects rather
// than silently memory-restoring; a filesystem-only snapshot already
// cold-boots from its own metadata, so the RPC stays unchanged for it.
func resolveFilesystemBoot(ctx context.Context, flags featureFlagsClient, memory *bool, snap queries.Snapshot) (bool, *api.APIError) {
	if memory == nil || *memory {
		return false, nil
	}

	if snapshotIsFilesystemOnly(snap) {
		return false, nil
	}

	if !flags.BoolFlag(ctx, featureflags.FsOnlyResumeAPIFlag,
		featureflags.TeamContext(snap.TeamID.String()),
		featureflags.SandboxContext(snap.SandboxID),
	) {
		return false, &api.APIError{
			Code:      http.StatusBadRequest,
			ErrorCode: errCodeMemoryOverrideDisabled,
			ClientMsg: "Resuming without memory (memory: false) is not enabled for this team; a plain resume still restores memory",
			Err:       fmt.Errorf("fs-only resume of memory snapshot '%s' rejected: feature disabled", snap.SandboxID),
		}
	}

	return true, nil
}

// buildResumeSandboxDataFromSnapshot returns a SandboxDataFetcher that fetches
// snapshot data for snapshotSandboxID from the cache and builds SandboxMetadata
// for resume operations. sandboxID is the ID the sandbox will run under — it
// differs from snapshotSandboxID when forking — and scopes the envd access token.
// The returned callback is called inside the sandbox lock to prevent race conditions.
func (a *APIStore) buildResumeSandboxDataFromSnapshot(snapshotSandboxID, sandboxID string, autoPauseOverride, memory *bool) orchestrator.SandboxDataFetcher {
	return func(ctx context.Context) (orchestrator.SandboxMetadata, *api.APIError) {
		lastSnapshot, err := a.snapshotCache.Get(ctx, snapshotSandboxID)
		if err != nil {
			return orchestrator.SandboxMetadata{}, &api.APIError{
				Code:      http.StatusInternalServerError,
				ClientMsg: "Error when getting snapshot",
				Err:       fmt.Errorf("error getting last snapshot for sandbox '%s': %w", snapshotSandboxID, err),
			}
		}

		snap := lastSnapshot.Snapshot
		build := lastSnapshot.EnvBuild

		// Resolved here rather than in the handler so the decision reads the
		// same locked snapshot fetch the create request is built from.
		filesystemBoot, apiErr := resolveFilesystemBoot(ctx, a.featureFlags, memory, snap)
		if apiErr != nil {
			return orchestrator.SandboxMetadata{}, apiErr
		}

		nodeID := snap.OriginNodeID

		alias := ""
		if len(lastSnapshot.Aliases) > 0 {
			alias = lastSnapshot.Aliases[0]
		}

		var envdAccessToken *string
		if snap.EnvSecure {
			accessToken, tokenErr := a.getEnvdAccessToken(build.EnvdVersion, sandboxID)
			if tokenErr != nil {
				return orchestrator.SandboxMetadata{}, tokenErr
			}
			envdAccessToken = &accessToken
		}

		autoPause := snap.AutoPause
		if autoPauseOverride != nil {
			autoPause = *autoPauseOverride
		}

		var network *types.SandboxNetworkConfig
		var autoResume *types.SandboxAutoResumeConfig
		var volumes []*types.SandboxVolumeMountConfig
		// Unlike auto_pause (which resume can override via the request body), the
		// auto-pause snapshot kind is intentionally always inherited from the
		// snapshot: there is no resume-time override for it. Changing the kind
		// requires creating a new sandbox with the desired autoPauseMemory.
		var autoPauseFilesystemOnly bool
		// A fork (snapshotSandboxID != sandboxID) inherits the parent sandbox's IAM
		// configuration from the snapshot, the same as a resume. The resumed/forked
		// execution still gets a freshly generated execution ID upstream, so no
		// stored identity subject is carried across.
		var iam *types.SandboxIam
		if snap.Config != nil {
			network = snap.Config.Network
			autoResume = snap.Config.AutoResume
			volumes = snap.Config.VolumeMounts
			autoPauseFilesystemOnly = snap.Config.AutoPauseFilesystemOnly
			iam = snap.Config.Iam
		}

		return orchestrator.SandboxMetadata{
			Metadata:                snap.Metadata,
			Build:                   build,
			AllowInternetAccess:     snap.AllowInternetAccess,
			Network:                 network,
			Alias:                   alias,
			TemplateID:              snap.EnvID,
			BaseTemplateID:          snap.BaseEnvID,
			AutoPause:               autoPause,
			AutoPauseFilesystemOnly: autoPauseFilesystemOnly,
			AutoResume:              autoResume,
			VolumeMounts:            convertDatabaseMountsToOrchestratorMounts(volumes),
			EnvdAccessToken:         envdAccessToken,
			Iam:                     iam,
			NodeID:                  &nodeID,
			SnapshotSandboxID:       snapshotSandboxID,
			FilesystemBoot:          filesystemBoot,
			FilesystemOnlySnapshot:  snapshotIsFilesystemOnly(snap),
		}, nil
	}
}
```

### 3.5 Pause and Checkpoint (API side)

Both first `UpsertSnapshot` in Postgres, which creates (once per sandbox) a snapshot template ID
(`id.Generate()`), and **a new env_build row whose UUID becomes the `build_id` sent to Pause/Checkpoint**.
Pause sends `template_id` = snapshot template ID, `build_id` = new UUID. Checkpoint sends `build_id` and
`metadata{template_id}`; the orchestrator pauses, snapshots and resumes the same VM under the same execution ID.

`packages/api/internal/orchestrator/pause_instance.go` lines 1-184 (of 184)

```go
package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/gogo/status"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"

	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/nodemanager"
	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/db/queries"
	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/id"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

// Defined in the sandbox package so the evictor can classify it without an
// import cycle.
type PauseQueueExhaustedError = sandbox.PauseQueueExhaustedError

func (o *Orchestrator) pauseSandbox(ctx context.Context, node *nodemanager.Node, sbx sandbox.Sandbox, filesystemOnly bool, restoreOnRefusal bool) error {
	ctx, span := tracer.Start(ctx, "pause-sandbox")
	defer span.End()

	result, err := o.throttledUpsertSnapshot(ctx, buildUpsertSnapshotParams(sbx, node, filesystemOnly))
	if err != nil {
		telemetry.ReportCriticalError(ctx, "error inserting snapshot for env", err)

		return err
	}

	// The snapshot's CPU info is pinned to the source build (see
	// buildUpsertSnapshotParams), so the node the pause physically ran on is not
	// persisted. Log it for debugging cross-generation pools.
	originNodeCPU := node.MachineInfo()
	logger.L().Info(ctx, "Snapshotting sandbox",
		logger.WithSandboxID(sbx.SandboxID),
		zap.String("origin_node_id", node.ID),
		zap.String("origin_node_cpu_architecture", originNodeCPU.CPUArchitecture),
		zap.String("origin_node_cpu_family", originNodeCPU.CPUFamily),
		zap.String("origin_node_cpu_model", originNodeCPU.CPUModel),
		zap.String("origin_node_cpu_model_name", originNodeCPU.CPUModelName),
		zap.Strings("origin_node_cpu_flags", originNodeCPU.CPUFlags),
		zap.String("source_build_id", sbx.BuildID.String()),
	)

	err = snapshotInstance(ctx, node, sbx, result.TemplateID, result.BuildID.String(), filesystemOnly, restoreOnRefusal)
	if err != nil {
		// The build is already committed, and nothing reaps one left non-terminal.
		o.failSnapshotBuild(ctx, result.BuildID, err)

		if errors.Is(err, PauseQueueExhaustedError{}) {
			telemetry.ReportEvent(ctx, "pause refused retryably", telemetry.WithSandboxID(sbx.SandboxID))

			return PauseQueueExhaustedError{}
		}

		telemetry.ReportCriticalError(ctx, "error pausing sandbox", err)

		return fmt.Errorf("error pausing sandbox: %w", err)
	}

	if err := o.finishSnapshotBuild(ctx, result.BuildID, types.BuildStatusSuccess); err != nil {
		telemetry.ReportCriticalError(ctx, "error pausing sandbox", err)

		return fmt.Errorf("error pausing sandbox: %w", err)
	}

	o.snapshotCache.Invalidate(context.WithoutCancel(ctx), sbx.SandboxID)

	return nil
}

func snapshotInstance(ctx context.Context, node *nodemanager.Node, sbx sandbox.Sandbox, templateID, buildID string, filesystemOnly bool, restoreOnRefusal bool) error {
	childCtx, childSpan := tracer.Start(ctx, "snapshot-instance")
	defer childSpan.End()

	client, childCtx := node.GetSandboxDeleteCtx(childCtx, sbx.SandboxID, sbx.ExecutionID, restoreOnRefusal)
	_, err := client.Sandbox.Pause(
		childCtx, &orchestrator.SandboxPauseRequest{
			SandboxId:      sbx.SandboxID,
			TemplateId:     templateID,
			BuildId:        buildID,
			FilesystemOnly: filesystemOnly,
		},
	)

	if err == nil {
		telemetry.ReportEvent(ctx, "Paused sandbox")

		return nil
	}

	st, ok := status.FromError(err)
	if !ok {
		return err
	}

	if st.Code() == codes.ResourceExhausted {
		logger.L().Warn(ctx, "Pause refused by the node", logger.WithSandboxID(sbx.SandboxID), zap.String("node_message", st.Message()))

		return PauseQueueExhaustedError{}
	}

	// Only the edge answers a pause with Aborted: the node refused and the
	// route could not be restored (a node never emits it).
	if st.Code() == codes.Aborted {
		logger.L().Warn(ctx, "Pause refused by the node but its route was lost", logger.WithSandboxID(sbx.SandboxID), zap.String("edge_message", st.Message()))

		return ErrRefusedRouteLost
	}

	return fmt.Errorf("failed to pause sandbox '%s': %w", sbx.SandboxID, err)
}

func (o *Orchestrator) WaitForStateChange(ctx context.Context, teamID uuid.UUID, sandboxID string) error {
	return o.sandboxStore.WaitForStateChange(ctx, teamID, sandboxID)
}

func buildUpsertSnapshotParams(sbx sandbox.Sandbox, node *nodemanager.Node, filesystemOnly bool) queries.UpsertSnapshotParams {
	metadata := types.JSONBStringMap(sbx.Metadata)
	if metadata == nil {
		metadata = types.JSONBStringMap{}
	}

	var clusterID *uuid.UUID
	if sbx.ClusterID != consts.LocalClusterID {
		clusterID = &sbx.ClusterID
	}

	return queries.UpsertSnapshotParams{
		// Used if there's no snapshot for this sandbox yet
		TemplateID:     id.Generate(),
		TeamID:         sbx.TeamID,
		ClusterID:      clusterID,
		BaseTemplateID: sbx.BaseTemplateID,
		SandboxID:      sbx.SandboxID,
		StartedAt:      pgtype.Timestamptz{Time: sbx.StartTime, Valid: true},
		Vcpu:           sbx.VCpu,
		RamMb:          sbx.RamMB,
		// We don't know this information
		FreeDiskSizeMb:      0,
		TotalDiskSizeMb:     &sbx.TotalDiskSizeMB,
		Metadata:            metadata,
		KernelVersion:       sbx.KernelVersion,
		FirecrackerVersion:  sbx.FirecrackerVersion,
		EnvdVersion:         &sbx.EnvdVersion,
		Secure:              sbx.EnvdAccessToken != nil,
		AllowInternetAccess: sbx.AllowInternetAccess,
		AutoPause:           sbx.AutoPause,
		Config: &types.PausedSandboxConfig{
			Version:                 types.PausedSandboxConfigVersion,
			Network:                 sbx.Network,
			AutoResume:              sbx.AutoResume,
			VolumeMounts:            sbx.VolumeMounts,
			FilesystemOnly:          filesystemOnly,
			AutoPauseFilesystemOnly: sbx.AutoPauseFilesystemOnly,
			Iam:                     sbx.Iam,
		},
		OriginNodeID: node.ID,
		Status:       types.BuildStatusSnapshotting,
		// Pin the snapshot's CPU info to the source build instead of the executing
		// node, so a pause/resume across CPU generations stays compatible.
		SourceBuildID: sbx.BuildID,
	}
}

// throttledUpsertSnapshot runs UpsertSnapshot gated by the snapshot upsert semaphore.
func (o *Orchestrator) throttledUpsertSnapshot(ctx context.Context, params queries.UpsertSnapshotParams) (queries.UpsertSnapshotRow, error) {
	if err := o.snapshotUpsertSem.Acquire(ctx, 1); err != nil {
		return queries.UpsertSnapshotRow{}, err
	}
	defer o.snapshotUpsertSem.Release(1)

	return o.sqlcDB.UpsertSnapshot(ctx, params)
}
```

`packages/api/internal/orchestrator/checkpoint_instance.go` lines 1-141 (of 141)

```go
package orchestrator

import (
	"context"
	"fmt"
	"sync"

	"github.com/gogo/status"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"

	"github.com/e2b-dev/infra/packages/api/internal/sandbox"
	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/storageopts"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

// CheckpointSandbox snapshots a running sandbox in place: the sandbox is
// briefly paused on its node, snapshotted, and resumed under the same
// execution ID, so it keeps running with its ID, expiration, and reservation
// untouched. The snapshot is written to the sandbox's own snapshots row, so
// it can immediately be resumed or forked from.
func (o *Orchestrator) CheckpointSandbox(ctx context.Context, teamID uuid.UUID, sandboxID string) error {
	ctx, span := tracer.Start(ctx, "checkpoint-sandbox")
	defer span.End()

	transition, alreadyDone, finishSnapshotting, err := o.sandboxStore.StartRemoving(ctx, teamID, sandboxID, sandbox.RemoveOpts{Action: sandbox.StateActionSnapshot})
	sbx := transition.Sandbox
	if err != nil {
		return fmt.Errorf("failed to start snapshotting: %w", err)
	}

	// alreadyDone conflates joining a concurrent checkpoint that just
	// succeeded with finding the sandbox stuck in Snapshotting after a failed
	// one, where no fresh snapshot exists. Treating it as success could fork
	// stale state, so report a conflict and let the caller retry (same
	// behavior as CreateSnapshotTemplate).
	if alreadyDone {
		return &sandbox.InvalidStateTransitionError{
			CurrentState: sandbox.StateSnapshotting,
			TargetState:  sandbox.StateSnapshotting,
		}
	}

	// finish completes the snapshotting transition exactly once.
	// On success (nil) it restores the sandbox to Running.
	// On error it leaves the state as Snapshotting so that
	// RemoveSandbox can transition directly to Killing.
	var once sync.Once
	finish := func(err error) {
		once.Do(func() {
			finishSnapshotting(context.WithoutCancel(ctx), err)
		})
	}
	defer finish(nil)

	node := o.getOrConnectNode(ctx, sbx.ClusterID, sbx.NodeID)
	if node == nil {
		return fmt.Errorf("node '%s' not found", sbx.NodeID)
	}

	upsertResult, err := o.throttledUpsertSnapshot(ctx, buildUpsertSnapshotParams(sbx, node, false))
	if err != nil {
		return fmt.Errorf("error upserting snapshot: %w", err)
	}

	// Checkpoint pauses the sandbox, snapshots it, and resumes it on the
	// orchestrator with the same ExecutionID. Once the pause has started, the
	// orchestrator stops the old sandbox itself on error; RemoveSandbox is
	// still needed to clean up API-side state (store, routing, analytics).
	client, childCtx := node.GetClient(ctx)
	_, err = client.Sandbox.Checkpoint(childCtx, &orchestrator.SandboxCheckpointRequest{
		SandboxId: sbx.SandboxID,
		BuildId:   upsertResult.BuildID.String(),
		Metadata:  map[string]string{storageopts.ObjectMetadataTemplateID: upsertResult.TemplateID},
	})
	if err != nil {
		// Cleanup must run even when the checkpoint failed because this
		// request's context was cancelled (e.g. client disconnect mid-fork).
		cleanupCtx := context.WithoutCancel(ctx)

		o.failSnapshotBuild(cleanupCtx, upsertResult.BuildID, err)

		// The orchestrator rejects these before pausing the VM (envd too old,
		// starting-sandboxes queue full), so the sandbox is still running
		// healthy on its node: restore it to Running instead of killing it.
		if st, ok := status.FromError(err); ok {
			switch st.Code() {
			case codes.FailedPrecondition:
				finish(nil)

				return fmt.Errorf("checkpoint rejected: %w", err)
			case codes.ResourceExhausted:
				logger.L().Warn(ctx, "Checkpoint refused by the node", logger.WithSandboxID(sandboxID), zap.String("node_message", st.Message()))
				finish(nil)

				return PauseQueueExhaustedError{}
			case codes.Canceled, codes.DeadlineExceeded:
				// Only when OUR ctx died (client disconnect): the code is then
				// generated by the gRPC client locally and masks the
				// orchestrator's real verdict — the checkpoint may have
				// succeeded, been rejected with the sandbox healthy, or
				// failed either way. Killing on unknown destroys a healthy
				// in-place sandbox (the node keeps it running through a
				// checkpoint); restoring at worst leaves a stale row that
				// expiry eviction cleans up. A Canceled that arrives WITHOUT
				// our ctx being done is the orchestrator's own and falls
				// through to the kill below.
				if ctx.Err() != nil {
					finish(nil)

					return fmt.Errorf("checkpoint abandoned by caller: %w", err)
				}
			}
		}

		// Complete the snapshotting transition with error — leaves state as
		// Snapshotting (no restore to Running) and clears the transition key
		// so RemoveSandbox can proceed without deadlock.
		finish(err)

		if killErr := o.RemoveSandbox(cleanupCtx, teamID, sandboxID, sandbox.RemoveOpts{Action: sandbox.StateActionKill}); killErr != nil {
			telemetry.ReportError(cleanupCtx, "error killing sandbox after failed checkpoint", killErr)
		}

		return fmt.Errorf("checkpoint failed: %w", err)
	}

	if err := o.finishSnapshotBuild(ctx, upsertResult.BuildID, types.BuildStatusSuccess); err != nil {
		return fmt.Errorf("error updating build status: %w", err)
	}

	o.snapshotCache.Invalidate(context.WithoutCancel(ctx), sandboxID)

	telemetry.ReportEvent(ctx, "Checkpointed sandbox")

	return nil
}
```

`packages/db/queries/create_new_snapshot.sql.go` lines 16-145 (of 180)

```go
const upsertSnapshot = `-- name: UpsertSnapshot :one
WITH new_template AS (
    INSERT INTO "public"."envs" (id, public, created_by, team_id, updated_at, source, cluster_id)
    SELECT $1, FALSE, NULL, $2, now(), 'snapshot', $3
    WHERE NOT EXISTS (
        SELECT id
        FROM "public"."snapshots" s
        WHERE s.sandbox_id = $4
    ) RETURNING id
),

snapshot as (
    INSERT INTO "public"."snapshots" (
       sandbox_id,
       base_env_id,
       team_id,
       env_id,
       metadata,
       sandbox_started_at,
       env_secure,
       allow_internet_access,
       origin_node_id,
       auto_pause,
       config,
       created_at
    )
    VALUES (
            $4,
            $5,
            $2,
            -- If snapshot already exists, new_template id will be null, env_id can't be null, so use placeholder ''
            COALESCE((SELECT id FROM new_template), ''),
            $6,
            $7,
            $8,
            $9,
            $10,
            $11,
            $12,
            now()
   )
    ON CONFLICT (sandbox_id) DO UPDATE SET
        metadata = excluded.metadata,
        sandbox_started_at = excluded.sandbox_started_at,
        allow_internet_access = COALESCE(excluded.allow_internet_access, snapshots.allow_internet_access),
        origin_node_id = excluded.origin_node_id,
        auto_pause = excluded.auto_pause,
        config = excluded.config
    RETURNING env_id as template_id
),

new_build as (
    INSERT INTO "public"."env_builds" (
        vcpu,
        ram_mb,
        free_disk_size_mb,
        kernel_version,
        firecracker_version,
        envd_version,
        status,
        cluster_node_id,
        total_disk_size_mb,
        updated_at,
        cpu_architecture,
        cpu_family,
        cpu_model,
        cpu_model_name,
        cpu_flags
    )
    VALUES (
        $13,
        $14,
        $15,
        $16,
        $17,
        $18,
        $19,
        $10,
        $20,
        now(),
        (SELECT eb.cpu_architecture FROM "public"."env_builds" eb WHERE eb.id = $21),
        (SELECT eb.cpu_family FROM "public"."env_builds" eb WHERE eb.id = $21),
        (SELECT eb.cpu_model FROM "public"."env_builds" eb WHERE eb.id = $21),
        (SELECT eb.cpu_model_name FROM "public"."env_builds" eb WHERE eb.id = $21),
        (SELECT eb.cpu_flags FROM "public"."env_builds" eb WHERE eb.id = $21)
    )
    RETURNING id as build_id
),

build_assignment as (
    INSERT INTO "public"."env_build_assignments" (env_id, build_id, tag)
    VALUES (
        (SELECT template_id FROM snapshot),
        (SELECT build_id FROM new_build),
        'default'
    )
    RETURNING build_id, env_id as template_id
)

SELECT build_id, template_id FROM build_assignment
`

type UpsertSnapshotParams struct {
	TemplateID          string
	TeamID              uuid.UUID
	ClusterID           *uuid.UUID
	SandboxID           string
	BaseTemplateID      string
	Metadata            types.JSONBStringMap
	StartedAt           pgtype.Timestamptz
	Secure              bool
	AllowInternetAccess *bool
	OriginNodeID        string
	AutoPause           bool
	Config              *types.PausedSandboxConfig
	Vcpu                int64
	RamMb               int64
	FreeDiskSizeMb      int64
	KernelVersion       string
	FirecrackerVersion  string
	EnvdVersion         *string
	Status              types.BuildStatus
	TotalDiskSizeMb     *int64
	SourceBuildID       uuid.UUID
}

type UpsertSnapshotRow struct {
	BuildID    uuid.UUID
	TemplateID string
}
```

Pause handler (explicit `POST /sandboxes/{id}/pause`; `memory:false` => filesystem-only):

`packages/api/internal/handlers/sandbox_pause.go` lines 45-134 (of 172)

```go
func (a *APIStore) PostSandboxesSandboxIDPause(c *gin.Context, sandboxID api.SandboxID) {
	ctx := c.Request.Context()
	// Get team from context, use TeamContextKey

	teamID := auth.MustGetTeamID(c)

	var err error
	sandboxID, err = utils.ShortID(sandboxID)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, "Invalid sandbox ID")

		return
	}

	span := trace.SpanFromContext(ctx)
	span.SetAttributes(telemetry.WithSandboxID(sandboxID))

	traceID := span.SpanContext().TraceID().String()
	c.Set("traceID", traceID)

	// The request body is optional — existing callers send none. Default to a
	// full memory snapshot; memory:false requests a filesystem-only snapshot.
	// ParseOptionalBody tolerates an absent/empty body and parses a present one
	// regardless of Content-Length (chunked requests report -1 even with a body).
	body, bindErr := ginutils.ParseOptionalBody[api.PostSandboxesSandboxIDPauseJSONRequestBody](ctx, c)
	if bindErr != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Error when parsing request: %s", bindErr))

		return
	}
	filesystemOnly := body.Memory != nil && !*body.Memory

	// Version-gate filesystem-only snapshots HERE, before the pause chain
	// commits: RemoveSandbox tears down routing and store state regardless of
	// the orchestrator RPC's outcome, so a refusal any later than this would
	// leave a live VM for the orphan reconciler to kill. Refused here, the
	// sandbox keeps running untouched. The check is EXACT — no flag
	// re-resolution — so for records carrying the orchestrator-resolved
	// version it cannot disagree with the orchestrator's own gate;
	backend := a.pauseBackend()

	pause.LogInitiated(ctx, sandboxID, teamID.String(), pause.ReasonRequest, filesystemOnly)

	err = backend.RemoveSandbox(ctx, teamID, sandboxID, sandbox.RemoveOpts{Action: sandbox.StateActionPause, FilesystemOnly: filesystemOnly})
	var transErr *sandbox.InvalidStateTransitionError

	switch {
	case err == nil:
		pause.LogSuccess(ctx, sandboxID, teamID.String(), pause.ReasonRequest, filesystemOnly)
	case errors.Is(err, orchestrator.ErrSandboxNotFound):
		apiErr := pauseHandleNotRunningSandbox(ctx, a.snapshotCache, sandboxID, teamID)
		switch apiErr.Code {
		case http.StatusConflict:
			pause.LogSkipped(ctx, sandboxID, teamID.String(), pause.ReasonRequest, pause.SkipReasonAlreadyPaused, filesystemOnly)
		case http.StatusNotFound:
			pause.LogSkipped(ctx, sandboxID, teamID.String(), pause.ReasonRequest, pause.SkipReasonNotFound, filesystemOnly)
		default:
			pause.LogFailure(ctx, sandboxID, teamID.String(), pause.ReasonRequest, filesystemOnly, err)
		}
		a.sendAPIStoreError(c, apiErr.Code, apiErr.ClientMsg)

		return
	case errors.As(err, &transErr):
		pause.LogFailure(ctx, sandboxID, teamID.String(), pause.ReasonRequest, filesystemOnly, err)
		a.sendAPIStoreError(c, http.StatusConflict, fmt.Sprintf("Sandbox '%s' cannot be paused while in '%s' state", sandboxID, transErr.CurrentState))

		return
	// Reached only after the API restored the sandbox, so the retry can succeed.
	case errors.Is(err, orchestrator.PauseQueueExhaustedError{}):
		pause.LogSkipped(ctx, sandboxID, teamID.String(), pause.ReasonRequest, pause.SkipReasonAdmissionRefused, filesystemOnly)
		a.sendAPIStoreError(c, http.StatusServiceUnavailable, fmt.Sprintf("Sandbox '%s' cannot be paused right now because its node is busy, please retry", sandboxID))

		return
	// The sandbox is untouched: another replica, or a retry here, can pause it.
	case errors.Is(err, orchestrator.ErrDraining):
		pause.LogSkipped(ctx, sandboxID, teamID.String(), pause.ReasonRequest, pause.SkipReasonDraining, filesystemOnly)
		a.sendAPIStoreError(c, http.StatusServiceUnavailable, fmt.Sprintf("Sandbox '%s' cannot be paused right now because the server is shutting down, please retry", sandboxID))

		return
	default:
		pause.LogFailure(ctx, sandboxID, teamID.String(), pause.ReasonRequest, filesystemOnly, err)
		telemetry.ReportError(ctx, "error pausing sandbox", err)

		a.sendAPIStoreError(c, http.StatusInternalServerError, "Error pausing sandbox")

		return
	}

	c.Status(http.StatusNoContent)
}
```

Kill and pause routing from the API to the node (`Delete` carries a kill reason string):

`packages/api/internal/orchestrator/delete_instance.go` lines 300-371 (of 429)

```go
func (o *Orchestrator) removeSandboxFromNode(
	ctx context.Context,
	sbx sandbox.Sandbox,
	stateAction sandbox.StateAction,
	reason sandbox.KillReason,
	filesystemOnly bool,
	restoreOnRefusal bool,
) error {
	ctx, span := tracer.Start(ctx, "remove-sandbox-from-node")
	defer span.End()

	node := o.getOrConnectNode(ctx, sbx.ClusterID, sbx.NodeID)
	if node == nil {
		fields := []zap.Field{
			logger.WithNodeID(sbx.NodeID),
		}
		if stateAction == sandbox.StateActionKill {
			fields = append(fields, zap.String("kill_reason", reason.String()))
		}

		logger.L().Error(ctx, "failed to get node", fields...)

		return fmt.Errorf("node '%s' not found", sbx.NodeID)
	}

	// For remote cluster nodes we are using gPRC metadata for routing registration instead
	if !node.IsClusterNode() {
		// Remove the sandbox resources after the sandbox is deleted
		err := o.routingCatalog.DeleteSandbox(ctx, sbx.SandboxID, sbx.ExecutionID)
		if err != nil {
			fields := []zap.Field{
				zap.Error(err),
				logger.WithSandboxID(sbx.SandboxID),
			}
			if stateAction == sandbox.StateActionKill {
				fields = append(fields, zap.String("kill_reason", reason.String()))
			}

			logger.L().Error(ctx, "error removing routing record from catalog", fields...)
		}
	}

	sbxlogger.I(sbx).Debug(ctx, "Removing sandbox",
		zap.Bool("auto_pause", sbx.AutoPause),
		zap.String("state_action", stateAction.Name),
	)

	switch stateAction {
	case sandbox.StateActionPause:
		err := o.pauseSandbox(ctx, node, sbx, filesystemOnly, restoreOnRefusal)
		if err != nil {
			if dberrors.IsForeignKeyViolation(err) {
				killErr := o.killSandboxOnNode(ctx, node, sbx.ToNodeSandbox(), sandbox.KillReasonBaseTemplateMissing)
				logger.L().Error(ctx, "Pause failed due to missing base template, killed sandbox as fallback",
					logger.WithSandboxID(sbx.SandboxID),
					zap.String("base_template_id", sbx.BaseTemplateID),
					zap.String("kill_reason", sandbox.KillReasonBaseTemplateMissing.String()),
					zap.NamedError("pause_error", err),
					zap.NamedError("kill_error", killErr),
				)

				return fmt.Errorf("failed to pause sandbox '%s': base template no longer exists: %w", sbx.SandboxID, err)
			}

			return fmt.Errorf("failed to auto pause sandbox '%s': %w", sbx.SandboxID, err)
		}

		return nil
	case sandbox.StateActionKill:
		return o.killSandboxOnNode(ctx, node, sbx.ToNodeSandbox(), reason)
	}

```

`packages/api/internal/orchestrator/delete_instance.go` lines 400-420 (of 429)

```go
	node *nodemanager.Node,
	sbx sandbox.NodeSandbox,
	reason sandbox.KillReason,
) error {
	killReason := reason.String()
	req := &orchestrator.SandboxDeleteRequest{
		SandboxId:  sbx.SandboxID,
		KillReason: &killReason,
	}

	client, ctx := node.GetSandboxDeleteCtx(ctx, sbx.SandboxID, sbx.ExecutionID, false)
	_, err := client.Sandbox.Delete(ctx, req)
	st, ok := status.FromError(err)
	if ok && st.Code() == codes.NotFound {
		logger.L().Info(ctx, "Sandbox not found during kill",
			logger.WithSandboxID(sbx.SandboxID),
			logger.WithNodeID(node.ID),
			zap.String("kill_reason", killReason),
		)
	} else if err != nil {
		return fmt.Errorf("failed to delete sandbox: %w", err)
```

`packages/api/internal/sandbox/sandboxtypes/states.go` lines 50-68 (of 118)

```go

const (
	KillReasonUnknown             KillReason = "unknown"
	KillReasonRequest             KillReason = "request"
	KillReasonTimeout             KillReason = "timeout"
	KillReasonAdmin               KillReason = "admin"
	KillReasonOrphaned            KillReason = "orphaned"
	KillReasonBaseTemplateMissing KillReason = "base_template_missing"
)

// String returns the reason as a string, normalizing the empty value to
// "unknown". Keeps API-side log/metric fields consistent with the
// orchestrator-side normalization in pkg/server/sandboxes.go.
func (r KillReason) String() string {
	if r == "" {
		return string(KillReasonUnknown)
	}

	return string(r)
```

Timeout extension and network update use `Update`:

`packages/api/internal/orchestrator/update_instance.go` lines 30-60 (of 60)

```go
		),
	)
	defer span.End()

	node := o.getOrConnectNode(ctx, clusterID, nodeID)
	if node == nil {
		return fmt.Errorf("node '%s' not found", nodeID)
	}

	client, ctx := node.GetClient(ctx)
	_, err := client.Sandbox.Update(
		ctx, &orchestrator.SandboxUpdateRequest{
			SandboxId: sandboxID,
			EndTime:   timestamppb.New(endTime),
		},
	)
	if err != nil {
		grpcErr, ok := status.FromError(err)
		if ok && grpcErr.Code() == codes.NotFound {
			return ErrSandboxNotFound
		}

		err = utils.UnwrapGRPCError(err)

		return fmt.Errorf("failed to update sandbox '%s': %w", sandboxID, err)
	}

	telemetry.ReportEvent(ctx, "Updated sandbox")

	return nil
}
```

`packages/api/internal/orchestrator/update_network.go` lines 100-120 (of 130)

```go
			Code:      http.StatusInternalServerError,
			ClientMsg: fmt.Sprintf("Node hosting sandbox '%s' not found", sbx.SandboxID),
			Err:       fmt.Errorf("node '%s' not found for cluster '%s'", sbx.NodeID, sbx.ClusterID),
		}
	}

	client, ctx := node.GetClient(ctx)
	_, err := client.Sandbox.Update(ctx, &orchestratorgrpc.SandboxUpdateRequest{
		SandboxId: sbx.SandboxID,
		Egress:    egress,
	})
	if err != nil {
		grpcErr, ok := status.FromError(err)
		if ok && grpcErr.Code() == codes.NotFound {
			return &api.APIError{Code: http.StatusNotFound, ClientMsg: utils.SandboxNotFoundMsg(sbx.SandboxID), Err: err}
		}

		err = utils.UnwrapGRPCError(err)
		telemetry.ReportCriticalError(ctx, "failed to update sandbox network on node", err)

		return &api.APIError{
```

(Commentary) `end_time` enforcement: the orchestrator stores `end_time` (and `Update` moves it) and the
reboot path rejects an end time in the past, but the timeout kill/auto-pause is driven by the API's
evictor loop (`packages/api/internal/orchestrator/evictor/evict.go`, 50 ms poll) calling
Delete/Pause. A client that replaces the API must run its own expiry loop.

`packages/api/internal/orchestrator/evictor/evict.go` lines 23-26 (of 326)

```go
const (
	pollInterval               = 50 * time.Millisecond
	concurrencyRefreshInterval = 30 * time.Second
)
```

### 3.6 Orchestrator side of Create / Pause / Checkpoint / Delete / Update (`packages/orchestrator/pkg/server/sandboxes.go`)

Every sandbox (fresh create, resume, fork) is started by resuming the build's snapshot
(`ResumeSandbox`) unless the snapshot is filesystem-only or `filesystem_boot` is demanded
(`RebootSandbox`, a cold boot). The template's files are located by `build_id` alone; the `snapshot`
flag only changes admission (wait vs. fail fast on the starting semaphore), the NFS cache choice, the
resume-time envd upgrade and the emitted event type. Request timeout is 60 s.

`packages/orchestrator/pkg/server/sandboxes.go` lines 50-56 (of 2570)

```go
var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/orchestrator/pkg/server")

const (
	requestTimeout = 60 * time.Second
	// acquireTimeout is the max time to wait for a semaphore for resuming sandboxes snapshot.
	acquireTimeout = 15 * time.Second

```

`packages/orchestrator/pkg/server/sandboxes.go` lines 127-476 (of 2570)

```go
func (s *Server) Create(ctx context.Context, req *orchestrator.SandboxCreateRequest) (_ *orchestrator.SandboxCreateResponse, createErr error) {
	releaseWork := s.info.TrackWork()
	defer releaseWork()

	// set max request timeout for this request. The pre-boot journal replay runs
	// within this budget (a successful replay is fast; the cancel-immune worst case
	// is bounded well under it), so the orchestrator still times out before the
	// caller does and a recovery overrun surfaces as a retryable per-node error.
	ctx, cancel := context.WithTimeoutCause(ctx, requestTimeout, errors.New("request timed out"))
	defer cancel()

	// set up tracing
	ctx, childSpan := tracer.Start(ctx, "sandbox-create")
	defer childSpan.End()

	isResume := req.GetSandbox().GetSnapshot()
	// fsOnly reports the ARTIFACT kind (filesystem-only snapshot), mirroring the
	// fs_only pause label, so the historical fs-only latency cohort stays pure.
	// Combined with fs_boot_requested the population decomposes fully: the boot
	// path taken is fs_only OR fs_boot_requested; a rescue of a memory snapshot
	// is fs_only=false, fs_boot_requested=true. Set after metadata loads below.
	var fsOnly bool
	// filesystemBooted is the dispatch outcome (the reboot path ran); echoed in
	// the response so a demanding caller can verify the demand was honored.
	var filesystemBooted bool
	// Paired with success on the metric below so a replayed-then-hung start
	// (fs_recovery=replayed, success=false) is countable; neither signal shows it
	// alone. Stays "none" when no recovery ran (memory resume, flag off).
	fsRecovery := rootfs.RecoverOutcomeNone
	createStart := time.Now()
	// Set by maybeUpgradeEnvd below; labels the resume-latency histogram so the
	// treated (upgraded) vs untreated cohorts can be compared during the rollout.
	var envdUpgraded bool
	defer func() {
		s.sandboxCreateDuration.Record(ctx, time.Since(createStart).Milliseconds(),
			metric.WithAttributes(
				attribute.Bool("sandbox.resume", isResume),
				attribute.Bool("fs_only", fsOnly),
				attribute.Bool("fs_boot_requested", req.GetFilesystemBoot()),
				attribute.Bool("success", createErr == nil),
				attribute.Bool("envd.upgraded", envdUpgraded),
				attribute.String("fs_recovery", string(fsRecovery)),
			),
		)
	}()

	childSpan.SetAttributes(
		telemetry.WithBuildID(req.GetSandbox().GetBuildId()),
		telemetry.WithTeamID(req.GetSandbox().GetTeamId()),
		telemetry.WithTemplateID(req.GetSandbox().GetTemplateId()),
		telemetry.WithKernelVersion(req.GetSandbox().GetKernelVersion()),
		telemetry.WithSandboxID(req.GetSandbox().GetSandboxId()),
		telemetry.WithEnvdVersion(req.GetSandbox().GetEnvdVersion()),
	)

	// setup launch darkly
	ctx = featureflags.AddToContext(
		ctx,
		ldcontext.NewBuilder(req.GetSandbox().GetSandboxId()).
			Kind(featureflags.SandboxKind).
			SetString(featureflags.SandboxTemplateAttribute, req.GetSandbox().GetTemplateId()).
			SetString(featureflags.SandboxKernelVersionAttribute, req.GetSandbox().GetKernelVersion()).
			SetString(featureflags.SandboxFirecrackerVersionAttribute, req.GetSandbox().GetFirecrackerVersion()).
			SetString(featureflags.SandboxEnvdVersionAttribute, req.GetSandbox().GetEnvdVersion()).
			Build(),
		ldcontext.NewBuilder(req.GetSandbox().GetTeamId()).
			Kind(featureflags.TeamKind).
			Build(),
	)

	// BYOP egress proxy kill-switch; mirrors the API gate for direct gRPC
	// callers and snapshot resumes.
	if req.GetSandbox().GetNetwork().GetEgress().GetEgressProxyAddress() != "" {
		if !s.featureFlags.BoolFlag(ctx, featureflags.BYOPProxyEnabledFlag) {
			telemetry.ReportEvent(ctx, "egressProxy rejected by BYOPProxyEnabledFlag")

			return nil, status.Error(codes.PermissionDenied,
				"egress proxy is not enabled for this team")
		}
		if !s.sandboxFactory.EgressProxy().SupportsBYOP() {
			telemetry.ReportEvent(ctx, "egressProxy rejected: orchestrator build has no BYOP dialer")

			return nil, status.Error(codes.Unimplemented,
				"egress proxy is not supported by this orchestrator build")
		}
	}

	reservation, err := s.sandboxFactory.Sandboxes.Reserve(req.GetSandbox().GetSandboxId())
	if err != nil {
		return nil, s.sandboxAlreadyRunning(ctx, req.GetSandbox().GetSandboxId(), req.GetSandbox().GetExecutionId(), err)
	}
	var rollback *sandbox.Cleanup
	defer func() {
		s.finishSandboxStart(ctx, reservation, rollback, createErr)
	}()

	maxRunningSandboxesPerNode := s.info.MaxSandboxes.Load()

	runningSandboxes := int64(s.sandboxFactory.Sandboxes.Count())
	if runningSandboxes >= maxRunningSandboxesPerNode {
		telemetry.ReportEvent(ctx, "max number of running sandboxes reached")

		return nil, status.Errorf(codes.ResourceExhausted, "max number of running sandboxes on node reached (%d), please retry", maxRunningSandboxesPerNode)
	}

	// Check if we've reached the max number of starting instances on this node
	if req.GetSandbox().GetSnapshot() {
		err := s.waitForAcquire(ctx)
		if err != nil {
			return nil, err
		}
	} else {
		acquired := s.startingSandboxes.TryAcquire(1)
		if !acquired {
			telemetry.ReportEvent(ctx, "too many starting sandboxes on node")

			return nil, status.Errorf(codes.ResourceExhausted, "too many sandboxes starting on this node, please retry")
		}
	}
	defer s.startingSandboxes.Release(1)

	// Pinned: eviction Closes a template, which deletes its snapfile/metafile
	// from disk, so a template backing a running sandbox must not be evictable.
	// The pin is taken atomically with the lookup — taking it afterwards can
	// race an eviction already in flight. Released either by the rollback below
	// (if we never reach the lifecycle goroutine) or by that goroutine once the
	// sandbox has closed; releaseTemplate is idempotent, so registering it on
	// both paths still releases exactly one pin.
	template, releaseTemplate, err := s.templateCache.GetTemplatePinned(
		ctx,
		req.GetSandbox().GetBuildId(),
		req.GetSandbox().GetSnapshot(),
		false,
		sbxtemplate.GetTemplateOpts{MaxSandboxLengthHours: req.GetSandbox().GetMaxSandboxLength()},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get template snapshot data: %w", err)
	}

	rollback = sandbox.NewCleanup()
	rollback.AddNoContext(ctx, func() error {
		releaseTemplate()

		return nil
	})

	// Clone the network config to avoid modifying the original request
	network := proto.CloneOf(req.GetSandbox().GetNetwork())

	resolvedFCVersion := featureflags.ResolveFirecrackerVersion(ctx, s.featureFlags, req.GetSandbox().GetFirecrackerVersion())
	volumeMounts, err := createVolumeMountModelsFromAPI(req.GetSandbox().GetVolumeMounts())
	if err != nil {
		return nil, fmt.Errorf("failed to convert volume mounts: %w", err)
	}

	config := sandbox.NewConfig(sandbox.Config{
		BaseTemplateID: req.GetSandbox().GetBaseTemplateId(),

		Vcpu:            req.GetSandbox().GetVcpu(),
		RamMB:           req.GetSandbox().GetRamMb(),
		TotalDiskSizeMB: req.GetSandbox().GetTotalDiskSizeMb(),
		HugePages:       req.GetSandbox().GetHugePages(),

		Network: network,

		Envd: sandbox.EnvdMetadata{
			Version:     req.GetSandbox().GetEnvdVersion(),
			AccessToken: req.GetSandbox().EnvdAccessToken,
			Vars:        req.GetSandbox().GetEnvVars(),
		},

		FirecrackerConfig: fc.Config{
			KernelVersion:      req.GetSandbox().GetKernelVersion(),
			FirecrackerVersion: resolvedFCVersion,
		},

		VolumeMounts:          volumeMounts,
		MaxSandboxLengthHours: req.GetSandbox().GetMaxSandboxLength(),
	})
	childSpan.SetAttributes(
		telemetry.WithFirecrackerVersion(config.FirecrackerConfig.FirecrackerVersion),
	)

	runtime := sandboxtypes.RuntimeMetadata{
		TemplateID:  req.GetSandbox().GetTemplateId(),
		SandboxID:   req.GetSandbox().GetSandboxId(),
		ExecutionID: req.GetSandbox().GetExecutionId(),
		TeamID:      req.GetSandbox().GetTeamId(),
		BuildID:     req.GetSandbox().GetBuildId(),
		SandboxType: sandboxtypes.SandboxTypeSandbox,
	}

	meta, err := template.Metadata()
	if err != nil {
		return nil, fmt.Errorf("failed to read template metadata: %w", err)
	}

	fsOnly = meta.IsFilesystemOnly()
	filesystemBooted = filesystemBoot(meta, req)

	var sbx *sandbox.Sandbox
	if filesystemBooted {
		sbx, err = s.sandboxFactory.RebootSandbox(
			ctx,
			template,
			config,
			runtime,
			req.GetEndTime().AsTime(),
			req.GetSandbox(),
			// Defer routing until after the resume-time envd upgrade's
			// post-/init, so the sandbox isn't reachable during its pre-init
			// auth window. Promoted below via markSandboxLive.
			true,
			req.GetFilesystemBoot(),
			func(o rootfs.RecoverOutcome) { fsRecovery = o },
		)
	} else {
		sbx, err = s.sandboxFactory.ResumeSandbox(
			ctx,
			template,
			config,
			runtime,
			req.GetStartTime().AsTime(),
			req.GetEndTime().AsTime(),
			req.GetSandbox(),
			// Defer routing until after the resume-time envd upgrade's
			// post-/init (see markSandboxLive below).
			sandbox.WithDeferredLiveRegistration(),
		)
	}
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			// Snapshot data not found, let the API know the data aren't probably upload yet
			telemetry.ReportError(ctx, "sandbox files not found", err, telemetry.WithSandboxID(req.GetSandbox().GetSandboxId()))

			return nil, status.Errorf(codes.FailedPrecondition, "sandbox files for '%s' not found", req.GetSandbox().GetSandboxId())
		}

		err = errors.Join(err, context.Cause(ctx))
		telemetry.ReportCriticalError(ctx, "failed to create sandbox", err)
		logger.L().Error(ctx, "failed to create sandbox", zap.Error(err),
			zap.Bool("filesystem_boot_requested", req.GetFilesystemBoot()),
			logger.WithSandboxID(runtime.SandboxID),
			logger.WithBuildID(runtime.BuildID),
			logger.WithTemplateID(runtime.TemplateID),
			logger.WithEnvdVersion(config.Envd.Version),
			logger.WithKernelVersion(config.FirecrackerConfig.KernelVersion),
			logger.WithFirecrackerVersion(config.FirecrackerConfig.FirecrackerVersion),
		)

		return nil, status.Errorf(codes.Internal, "failed to create sandbox: %s", err)
	}

	rollback.Add(ctx, func(ctx context.Context) error { return stopAndCloseSandbox(ctx, sbx) })
	s.setupSandboxLifecycle(ctx, sbx, releaseTemplate)

	// Resume-time envd live-upgrade. The API /resume maps to Create
	// with snapshot=true, so this is the real resume path. Flag-driven,
	// best-effort + recover-wrapped (see maybeUpgradeEnvd) so it can't disrupt
	// resume. ctx already carries the LD context (envd-version/team/template).
	if req.GetSandbox().GetSnapshot() {
		var upErr error
		envdUpgraded, upErr = s.maybeUpgradeEnvd(ctx, sbx)
		if upErr != nil {
			sbx.SetStopReason(sandbox.StopReasonKilled)

			return nil, upErr
		}
	}

	// Promote to the live registry only now — after any resume-time envd upgrade
	// has run its post-/init and restored the access token — so the sandbox is
	// never routable during the upgrade's sub-second pre-init auth window. Both
	// the resume and reboot paths above defer this.
	if err := s.markSandboxLive(ctx, sbx, reservation); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to register sandbox: %s", err)
	}
	// Read off the start path; unknown here means the read has not landed yet.
	childSpan.SetAttributes(attribute.String("balloon_mode", sbx.BalloonMode()))

	// Read scheduling metadata after the sandbox resumed so the template's
	// memfile/rootfs devices (and their headers) are resolved.
	var schedulingMetadata *orchestrator.SchedulingMetadata
	if provider, ok := template.(interface {
		SchedulingMetadata(ctx context.Context) *orchestrator.SchedulingMetadata
	}); ok {
		schedulingMetadata = provider.SchedulingMetadata(ctx)
	}

	eventType := events.SandboxCreatedEventPair
	if req.GetSandbox().GetSnapshot() {
		eventType = events.SandboxResumedEventPair
	}

	teamID, buildId, eventsTTLDays, eventData := s.prepareSandboxEventData(ctx, sbx)
	s.publishEventAsync(
		ctx,
		teamID,
		events.SandboxEvent{
			Version:   events.StructureVersionV2,
			ID:        uuid.New(),
			Type:      eventType.Type,
			Timestamp: time.Now().UTC(),

			EventData:          eventData,
			SandboxID:          sbx.Runtime.SandboxID,
			SandboxExecutionID: sbx.Runtime.ExecutionID,
			SandboxTemplateID:  sbx.Config.BaseTemplateID,
			SandboxBuildID:     buildId,
			SandboxTeamID:      teamID,
			EventsTTLDays:      eventsTTLDays,
		},
	)

	sbx.SetExecutionStartedAt(time.Now())

	return &orchestrator.SandboxCreateResponse{
		ClientId:              s.info.ClientId,
		SchedulingMetadata:    schedulingMetadata,
		FilesystemBootApplied: filesystemBooted,
		// The resolved Firecracker version the sandbox actually runs, frozen
		// for its lifetime. The API stores it so a version-gated feature can
		// key off the running binary exactly instead of re-resolving the
		// flag, which drifts from this frozen value whenever the flag moves.
		ResolvedFirecrackerVersion: resolvedFCVersion,
	}, nil
}

func createVolumeMountModelsFromAPI(volumeMounts []*orchestrator.SandboxVolumeMount) ([]sandbox.VolumeMountConfig, error) {
	var errs []error

	results := make([]sandbox.VolumeMountConfig, 0, len(volumeMounts))

	for _, v := range volumeMounts {
		volumeID, err := uuid.Parse(v.GetId())
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid volume id %q: %w", v.GetId(), err))

			continue
		}

		results = append(results, sandbox.VolumeMountConfig{
			ID:   volumeID,
			Name: v.GetName(),
			Path: v.GetPath(),
			Type: v.GetType(),
		})
	}

	return results, errors.Join(errs...)
```

`packages/orchestrator/pkg/server/sandboxes.go` lines 479-540 (of 2570)

```go
func (s *Server) Update(ctx context.Context, req *orchestrator.SandboxUpdateRequest) (*emptypb.Empty, error) {
	releaseWork := s.info.TrackWork()
	defer releaseWork()

	ctx, childSpan := tracer.Start(ctx, "sandbox-update")
	defer childSpan.End()

	childSpan.SetAttributes(
		telemetry.WithSandboxID(req.GetSandboxId()),
	)

	sbx, ok := s.sandboxFactory.Sandboxes.Get(req.GetSandboxId())
	if !ok {
		telemetry.ReportCriticalError(ctx, "sandbox not found", nil)

		return nil, status.Error(codes.NotFound, "sandbox not found")
	}

	childSpan.SetAttributes(
		telemetry.WithTeamID(sbx.Runtime.TeamID),
		telemetry.WithTemplateID(sbx.Runtime.TemplateID),
		telemetry.WithBuildID(sbx.Runtime.BuildID),
		telemetry.WithFirecrackerVersion(sbx.Config.FirecrackerConfig.FirecrackerVersion),
		telemetry.WithKernelVersion(sbx.Config.FirecrackerConfig.KernelVersion),
		telemetry.WithEnvdVersion(sbx.Config.Envd.Version),
		attribute.String("balloon_mode", sbx.BalloonMode()),
	)

	// Mirror the Create-side BYOP gates; defense-in-depth for direct gRPC
	// callers.
	if req.GetEgress().GetEgressProxyAddress() != "" {
		ctx = featureflags.AddToContext(ctx,
			ldcontext.NewBuilder(sbx.Runtime.TeamID).
				Kind(featureflags.TeamKind).
				Build(),
		)
		if !s.featureFlags.BoolFlag(ctx, featureflags.BYOPProxyEnabledFlag) {
			telemetry.ReportEvent(ctx, "egressProxy update rejected by BYOPProxyEnabledFlag")

			return nil, status.Error(codes.PermissionDenied,
				"egress proxy is not enabled for this team")
		}
		if !s.sandboxFactory.EgressProxy().SupportsBYOP() {
			telemetry.ReportEvent(ctx, "egressProxy update rejected: orchestrator build has no BYOP dialer")

			return nil, status.Error(codes.Unimplemented,
				"egress proxy is not supported by this orchestrator build")
		}
	}

	var updates []utils.UpdateFunc

	if req.GetEndTime() != nil {
		updates = append(updates, func(_ context.Context) (func(context.Context), error) {
			old := sbx.GetEndAt()
			sbx.SetEndAt(req.GetEndTime().AsTime())

			return func(_ context.Context) { sbx.SetEndAt(old) }, nil
		})
	}

	if req.GetEgress() != nil {
```

`packages/orchestrator/pkg/server/sandboxes.go` lines 654-760 (of 2570)

```go
func (s *Server) List(ctx context.Context, _ *emptypb.Empty) (*orchestrator.SandboxListResponse, error) {
	_, childSpan := tracer.Start(ctx, "sandbox-list")
	defer childSpan.End()

	items := s.sandboxFactory.Sandboxes.Items()

	sandboxes := make([]*orchestrator.RunningSandbox, 0, len(items))

	for _, sbx := range items {
		if sbx == nil {
			continue
		}

		// Build sandboxes are not owned by the API and must never show up here,
		// or the API would treat them as orphans and kill them. They are the only
		// sandboxes created without an APIStoredConfig.
		if sbx.APIStoredConfig == nil {
			continue
		}

		startedAt := sbx.GetStartedAt()
		sandboxes = append(sandboxes, &orchestrator.RunningSandbox{
			Config:      sbx.APIStoredConfig,
			ClientId:    s.info.ClientId,
			StartTime:   timestamppb.New(startedAt),
			EndTime:     timestamppb.New(sbx.GetEndAt()),
			SandboxId:   sbx.Runtime.SandboxID,
			TeamId:      sbx.Runtime.TeamID,
			ExecutionId: sbx.Runtime.ExecutionID,
			Vcpu:        sbx.Config.Vcpu,
			RamMb:       sbx.Config.RamMB,
		})
	}

	return &orchestrator.SandboxListResponse{
		Sandboxes: sandboxes,
	}, nil
}

func (s *Server) Delete(ctxConn context.Context, in *orchestrator.SandboxDeleteRequest) (*emptypb.Empty, error) {
	releaseWork := s.info.TrackWork()
	defer releaseWork()

	ctx, cancel := context.WithTimeoutCause(ctxConn, requestTimeout, errors.New("request timed out"))
	defer cancel()

	ctx, childSpan := tracer.Start(ctx, "sandbox-delete")
	defer childSpan.End()

	childSpan.SetAttributes(
		telemetry.WithSandboxID(in.GetSandboxId()),
	)

	sbx, ok := s.sandboxFactory.Sandboxes.Get(in.GetSandboxId())
	if !ok {
		telemetry.ReportCriticalError(ctx, "sandbox not found", nil, telemetry.WithSandboxID(in.GetSandboxId()))

		return nil, status.Errorf(codes.NotFound, "sandbox '%s' not found", in.GetSandboxId())
	}

	childSpan.SetAttributes(
		telemetry.WithTeamID(sbx.Runtime.TeamID),
		telemetry.WithTemplateID(sbx.Runtime.TemplateID),
		telemetry.WithBuildID(sbx.Runtime.BuildID),
		telemetry.WithFirecrackerVersion(sbx.Config.FirecrackerConfig.FirecrackerVersion),
		telemetry.WithKernelVersion(sbx.Config.FirecrackerConfig.KernelVersion),
		telemetry.WithEnvdVersion(sbx.Config.Envd.Version),
		attribute.String("balloon_mode", sbx.BalloonMode()),
	)

	// Mark the sandbox as stopping so it is excluded from live queries (Get, Items,
	// Count) but remains findable by IP (GetByHostPort) while the Firecracker
	// process finishes shutting down.
	// This prevents the sandbox from being synced to API again.
	marked := s.sandboxFactory.Sandboxes.MarkStopping(ctx, sbx.Runtime.SandboxID, sbx.LifecycleID)
	if !marked {
		telemetry.ReportCriticalError(ctx, "failed to mark sandbox as stopping", nil, telemetry.WithSandboxID(in.GetSandboxId()))

		return nil, status.Errorf(codes.Internal, "failed to delete sandbox '%s'", in.GetSandboxId())
	}

	killReason := in.GetKillReason()
	if killReason == "" {
		killReason = killReasonUnknown
	}

	sbxlogger.E(sbx).Info(ctx, "Killing sandbox", zap.String("kill_reason", killReason))

	sbx.SetStopReason(sandbox.StopReasonKilled)

	// Check health metrics before stopping the sandbox
	sbx.Checks.Healthcheck(ctx, true)

	// Start the cleanup in a goroutine—the initial kill request should be send as the first thing in stop, and at this point you cannot route to the sandbox anymore.
	// We don't wait for the whole cleanup to finish here.
	go func() {
		err := sbx.Stop(context.WithoutCancel(ctx))
		if err != nil {
			sbxlogger.I(sbx).Error(ctx, "error stopping sandbox",
				logger.WithSandboxID(in.GetSandboxId()),
				zap.String("kill_reason", killReason),
				zap.Error(err),
			)
		}
	}()

	s.emitSandboxKilled(ctx, sbx, killReason)
```

`packages/orchestrator/pkg/server/sandboxes.go` lines 892-1060 (of 2570)

```go
func (s *Server) Pause(ctx context.Context, in *orchestrator.SandboxPauseRequest) (resp *orchestrator.SandboxPauseResponse, err error) {
	releaseWork := s.info.TrackWork()
	defer releaseWork()

	ctx, childSpan := tracer.Start(ctx, "sandbox-pause")
	defer childSpan.End()

	// Record pause duration split by fs_only vs memory (the gRPC RPC metric
	// can't distinguish them) and success, so dashboards can scope pause
	// call-count / error-rate / latency to filesystem-only pauses.
	pauseStart := time.Now()
	defer func() {
		s.sandboxPauseDuration.Record(ctx, time.Since(pauseStart).Milliseconds(),
			metric.WithAttributes(
				attribute.Bool("fs_only", in.GetFilesystemOnly()),
				attribute.Bool("success", err == nil),
			),
		)
	}()

	childSpan.SetAttributes(
		telemetry.WithSandboxID(in.GetSandboxId()),
		telemetry.WithTemplateID(in.GetTemplateId()),
		telemetry.WithBuildID(in.GetBuildId()),
	)

	sbx, ok := s.sandboxFactory.Sandboxes.Get(in.GetSandboxId())
	if !ok {
		telemetry.ReportCriticalError(ctx, "sandbox not found", nil, telemetry.WithSandboxID(in.GetSandboxId()))

		return nil, status.Error(codes.NotFound, "sandbox not found")
	}

	ctx = featureflags.AddToContext(ctx, sandboxFlagContexts(sbx)...)

	childSpan.SetAttributes(
		telemetry.WithTeamID(sbx.Runtime.TeamID),
		telemetry.WithFirecrackerVersion(sbx.Config.FirecrackerConfig.FirecrackerVersion),
		telemetry.WithKernelVersion(sbx.Config.FirecrackerConfig.KernelVersion),
		telemetry.WithEnvdVersion(sbx.Config.Envd.Version),
		attribute.String("balloon_mode", sbx.BalloonMode()),
	)

	// Flag-gated admission pre-flight: refuse retryably BEFORE any destructive
	// step while the parent memfile header is still deduplicating.
	var latchedErr error
	if graceMs := s.featureFlags.IntFlag(ctx, featureflags.PauseAdmissionGraceMs); graceMs >= 0 {
		outcome, waited, admitErr := sbx.AwaitSnapshotAdmission(ctx, time.Duration(graceMs)*time.Millisecond, !in.GetFilesystemOnly())
		s.recordPauseAdmission(ctx, "pause", outcome, waited)
		switch {
		case errors.Is(admitErr, sandbox.ErrSnapshotAdmissionPending):
			sbxlogger.E(sbx).Warn(ctx, "Refusing pause: parent memfile header is still deduplicating", zap.Duration("waited", waited))

			return nil, status.Errorf(codes.ResourceExhausted, "node is busy persisting sandbox '%s', please retry", in.GetSandboxId())
		case admitErr != nil && outcome == "":
			// The caller's context ended mid-wait; nothing was decided and the
			// sandbox is untouched.
			return nil, status.FromContextError(admitErr).Err()
		case admitErr != nil:
			// Permanent: carried into the kill path below, which needs
			// MarkStopping first.
			latchedErr = admitErr
		}
	}

	marked := s.sandboxFactory.Sandboxes.MarkStopping(ctx, sbx.Runtime.SandboxID, sbx.LifecycleID)
	if !marked {
		telemetry.ReportCriticalError(ctx, "failed to mark sandbox as stopping", nil, telemetry.WithSandboxID(in.GetSandboxId()))

		return nil, status.Error(codes.Internal, "failed to pause sandbox")
	}

	sbxlogger.E(sbx).Info(ctx, "Pausing sandbox")

	// A latched in-place seal failure — or a parent memfile dedup that failed
	// for good, found by the pre-flight — means this sandbox can never be
	// validly persisted again. Kill it promptly and name the cause: refusing would NOT
	// preserve it — the API pause chain has already deleted the routing entry
	// and removes the store record regardless of this RPC's result, so a
	// refused pause leaves a live VM that the orphan reconciler kills ~20s
	// later, attributed to `orphaned` instead of the seal failure. An
	// in-request kill keeps the API's record consistent with reality and puts
	// the real cause in the error and the stop reason. The same policy covers
	// a seal that only fails while Pause waits on it below: the deferred stop
	// tears the sandbox down and the seal error is returned.
	persistErr := latchedErr
	if persistErr == nil {
		persistErr = sbx.EnsurePausable()
	}
	if persistErr != nil {
		telemetry.ReportCriticalError(ctx, "sandbox cannot be persisted, killing it", persistErr, telemetry.WithSandboxID(in.GetSandboxId()))
		sbx.SetStopReason(sandbox.StopReasonKilled)
		s.stopSandboxAsync(context.WithoutCancel(ctx), sbx)

		// This kill replaces a Delete-RPC death, so it must produce the same
		// terminal surfaces Delete does: the killed lifecycle event and the
		// kill counter, with a reason naming the seal failure. Without them
		// the event stream shows the sandbox start and then simply go silent
		// (MarkStopping above hides it from the orphan reconciler, so nothing
		// downstream fills the gap).
		s.emitSandboxKilled(ctx, sbx, killReasonSealFailed)

		return nil, status.Errorf(codes.Internal, "sandbox '%s' cannot be persisted, killing it: %s", in.GetSandboxId(), persistErr)
	}

	// Set before the snapshot, not after it succeeds: snapshotting suspends the
	// guest and can close the sandbox, which would read as a crash.
	sbx.SetStopReason(sandbox.StopReasonPaused)

	// Stop the old sandbox in background after we're done
	defer s.stopSandboxAsync(context.WithoutCancel(ctx), sbx)

	// Defer the rootfs reflink off the pause critical path when enabled: pause is a
	// suspend, so nothing reads the diff until a later resume (which waits on the
	// upload anyway). NBD provider only; falls back to synchronous export otherwise.
	deferRootfsExport := s.featureFlags.BoolFlag(ctx, featureflags.DeferRootfsExportFlag)

	// Fire and forget - upload completes in the background
	res, err := s.snapshotAndCacheSandbox(ctx, sbx, in.GetBuildId(), map[string]string{storage.ObjectMetadataTemplateID: in.GetTemplateId()}, storage.ObjectOriginPause, in.GetFilesystemOnly(), deferRootfsExport, false)
	if err != nil {
		telemetry.ReportCriticalError(ctx, "error snapshotting sandbox", err, telemetry.WithSandboxID(in.GetSandboxId()))

		return nil, status.Errorf(codes.Internal, "error snapshotting sandbox '%s': %s", in.GetSandboxId(), err)
	}

	s.uploadSnapshotAsync(ctx, sbx, res)

	// Best-effort: the local snapshot is now in the cache and the remote upload
	// has been kicked off above (still in flight). Harvest a resume page-fault
	// trace from a throwaway warm resume of the local snapshot and (when enabled)
	// persist it as a prefetch mapping for the next resume. Runs in the
	// background; never affects the pause result, and waits for the upload before
	// touching metadata. No-op unless the harvest flag is on. Reuse the object
	// metadata the snapshot was uploaded with so the re-upload can't drift.
	//
	// Skip it for a filesystem-only pause: that snapshot has no memory diff, so a
	// memory resume of it would just fail (the resume is reserved for memory
	// snapshots; fs-only is a reboot) — there is no memory working set to harvest.
	if !in.GetFilesystemOnly() {
		s.harvestResumePrefetchAsync(ctx, sbx, res, in.GetBuildId(), res.objectMetadata)
	}

	teamID, buildId, eventsTTLDays, eventData := s.prepareSandboxEventData(ctx, sbx)
	eventData[executionEventDataKey] = s.getSandboxExecutionData(sbx)

	eventType := events.SandboxPausedEventPair
	s.publishEventAsync(
		ctx,
		teamID,
		events.SandboxEvent{
			Version:   events.StructureVersionV2,
			ID:        uuid.New(),
			Type:      eventType.Type,
			Timestamp: time.Now().UTC(),

			EventData:          eventData,
			SandboxID:          sbx.Runtime.SandboxID,
			SandboxExecutionID: sbx.Runtime.ExecutionID,
			SandboxTemplateID:  sbx.Config.BaseTemplateID,
			SandboxBuildID:     buildId,
			SandboxTeamID:      teamID,
			EventsTTLDays:      eventsTTLDays,
		},
	)

	return &orchestrator.SandboxPauseResponse{
		SchedulingMetadata: res.schedulingMetadata,
	}, nil
}
```

`packages/orchestrator/pkg/server/sandboxes.go` lines 1155-1220 (of 2570)

```go
func (s *Server) Checkpoint(ctx context.Context, in *orchestrator.SandboxCheckpointRequest) (*orchestrator.SandboxCheckpointResponse, error) {
	releaseWork := s.info.TrackWork()
	defer releaseWork()

	ctx, childSpan := tracer.Start(ctx, "sandbox-checkpoint")
	defer childSpan.End()

	childSpan.SetAttributes(
		telemetry.WithSandboxID(in.GetSandboxId()),
		telemetry.WithBuildID(in.GetBuildId()),
	)

	sbx, ok := s.sandboxFactory.Sandboxes.Get(in.GetSandboxId())
	if !ok {
		telemetry.ReportCriticalError(ctx, "sandbox not found", nil, telemetry.WithSandboxID(in.GetSandboxId()))

		return nil, status.Errorf(codes.NotFound, "sandbox '%s' not found", in.GetSandboxId())
	}

	ctx = featureflags.AddToContext(ctx, sandboxFlagContexts(sbx)...)

	childSpan.SetAttributes(
		telemetry.WithTeamID(sbx.Runtime.TeamID),
		telemetry.WithTemplateID(sbx.Runtime.TemplateID),
		telemetry.WithFirecrackerVersion(sbx.Config.FirecrackerConfig.FirecrackerVersion),
		telemetry.WithKernelVersion(sbx.Config.FirecrackerConfig.KernelVersion),
		telemetry.WithEnvdVersion(sbx.Config.Envd.Version),
		// The stamp, so a refusal below still carries the cohort; the route
		// decision overwrites it with the device's answer.
		attribute.String("balloon_mode", sbx.BalloonMode()),
	)

	// Check envd version before snapshotting.
	if err := utils.CheckEnvdVersionForSnapshot(sbx.Config.Envd.Version); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "%s", err.Error())
	}

	// The same flag-gated admission pre-flight as Pause (a checkpoint always
	// takes a full memory snapshot, on both the in-place and resume-fresh
	// paths); before waitForAcquire so a grace wait never holds a start slot.
	if graceMs := s.featureFlags.IntFlag(ctx, featureflags.PauseAdmissionGraceMs); graceMs >= 0 {
		outcome, waited, admitErr := sbx.AwaitSnapshotAdmission(ctx, time.Duration(graceMs)*time.Millisecond, true)
		s.recordPauseAdmission(ctx, "checkpoint", outcome, waited)
		switch {
		case errors.Is(admitErr, sandbox.ErrSnapshotAdmissionPending):
			sbxlogger.E(sbx).Warn(ctx, "Refusing checkpoint: parent memfile header is still deduplicating", zap.Duration("waited", waited))

			return nil, status.Errorf(codes.ResourceExhausted, "node is busy persisting sandbox '%s', please retry", in.GetSandboxId())
		case admitErr != nil && outcome == "":
			return nil, status.FromContextError(admitErr).Err()
		case admitErr != nil:
			// Unlike Pause, nothing downstream re-checks a latched seal.
			sbxlogger.E(sbx).Warn(ctx, "Refusing checkpoint: sandbox cannot be persisted", zap.Error(admitErr))

			return nil, status.Errorf(codes.FailedPrecondition, "sandbox '%s' cannot be persisted: %s", in.GetSandboxId(), admitErr)
		}
	}

	// Acquire the starting semaphore before resuming, same as Create/Pause.
	if err := s.waitForAcquire(ctx); err != nil {
		return nil, err
	}
	defer s.startingSandboxes.Release(1)

	sbxlogger.E(sbx).Info(ctx, "Checkpointing sandbox")

```

`build_id` must parse as a UUID for a pause to work (`Sandbox.Pause`):

`packages/orchestrator/pkg/sandbox/sandbox.go` lines 1988-1995 (of 4288)

```go
func (s *Sandbox) Pause(
	ctx context.Context,
	m metadata.Template,
	useCase SnapshotUseCase,
	opts ...PauseOption,
) (st *Snapshot, e error) {
	var pauseOpts pauseOptions
	for _, opt := range opts {
```

`packages/orchestrator/pkg/sandbox/sandbox.go` lines 2014-2024 (of 4288)

```go
	cachePaths, err := storage.Paths{BuildID: m.Template.BuildID}.Cache(s.config.StorageConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create cache paths: %w", err)
	}
	cleanup.AddNoContext(ctx, cachePaths.Close)

	buildID, err := uuid.Parse(cachePaths.BuildID)
	if err != nil {
		return nil, fmt.Errorf("failed to parse build id: %w", err)
	}

```

### 3.7 Template build call path (API -> TemplateService)

`TemplateManager.CreateTemplate` builds `TemplateConfig` and calls `TemplateCreate`, then polls
`TemplateBuildStatus` every 1 s (up to 1 h) until `Completed` or `Failed`. On `Completed` the API stores
`metadata.rootfsSizeKey` as the build's `total_disk_size_mb`, `envdVersionKey` as `envd_version`, plus
the kernel/firecracker versions actually used; those values are what later go into `SandboxConfig`.

`packages/api/internal/template-manager/create_template.go` lines 1-349 (of 349)

```go
package template_manager

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	templatecache "github.com/e2b-dev/infra/packages/api/internal/cache/templates"
	"github.com/e2b-dev/infra/packages/api/internal/utils"
	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/shared/pkg/fcversion"
	templatemanagergrpc "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
	"github.com/e2b-dev/infra/packages/shared/pkg/id"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

type FromTemplateError struct {
	err     error
	message string
}

func (e *FromTemplateError) Error() string {
	return e.message
}

func (e *FromTemplateError) Unwrap() error {
	return e.err
}

func (tm *TemplateManager) CreateTemplate(
	ctx context.Context,
	teamID uuid.UUID,
	teamSlug string,
	templateID string,
	buildID uuid.UUID,
	kernelVersion,
	firecrackerVersion string,
	startCommand *string,
	vCpuCount,
	diskSizeMB,
	freeDiskSizeMB,
	memoryMB int64,
	readyCommand *string,
	fromImage *string,
	fromTemplate *string,
	fromImageRegistry *api.FromImageRegistry,
	force *bool,
	steps *[]api.TemplateStep,
	clusterID uuid.UUID,
	nodeID string,
	version string,
) (e error) {
	ctx, span := tracer.Start(ctx, "create-template",
		trace.WithAttributes(
			telemetry.WithTemplateID(templateID),
		),
	)
	defer span.End()

	defer func() {
		if e == nil {
			return
		}

		// Report build failure status on any error while creating the template
		telemetry.ReportCriticalError(ctx, "build failed", e, telemetry.WithTemplateID(templateID))
		err := tm.SetStatus(
			ctx,
			buildID,
			types.BuildStatusGroupFailed,
			&templatemanagergrpc.TemplateBuildStatusReason{
				Message: fmt.Sprintf("error when building env: %s", e),
			},
		)
		if err != nil {
			e = errors.Join(e, fmt.Errorf("failed to set build status to failed: %w", err))
		}
	}()

	features, err := fcversion.New(firecrackerVersion)
	if err != nil {
		return fmt.Errorf("failed to get features for firecracker version '%s': %w", firecrackerVersion, err)
	}

	client, err := tm.GetClusterBuildClient(clusterID, nodeID)
	if err != nil {
		return fmt.Errorf("failed to get builder: %w", err)
	}

	var startCmd string
	if startCommand != nil {
		startCmd = *startCommand
	}
	var readyCmd string
	if readyCommand != nil {
		readyCmd = *readyCommand
	}

	imageRegistry, err := convertImageRegistry(fromImageRegistry)
	if err != nil {
		return fmt.Errorf("failed to convert image registry: %w", err)
	}

	// TODO(ENG-3852): Remove later. KernelVersion and FirecrackerVersion are deprecated on
	// template-manager selects its own versions and reports the ones it actually
	// used via TemplateBuildMetadata. They are still populated here for
	// backwards compatibility with older template-managers that honor them.
	template := &templatemanagergrpc.TemplateConfig{
		TeamID:             teamID.String(),
		TemplateID:         templateID,
		BuildID:            buildID.String(),
		VCpuCount:          int32(vCpuCount),
		MemoryMB:           int32(memoryMB),
		DiskSizeMB:         int32(diskSizeMB),
		FreeDiskSizeMB:     new(int32(freeDiskSizeMB)),
		KernelVersion:      kernelVersion,
		FirecrackerVersion: firecrackerVersion,
		HugePages:          features.HasHugePages(),
		StartCommand:       startCmd,
		ReadyCommand:       readyCmd,
		Force:              force,
		Steps:              convertTemplateSteps(steps),
		FromImageRegistry:  imageRegistry,
	}

	err = setTemplateSource(ctx, tm, teamID, teamSlug, template, fromImage, fromTemplate)
	if err != nil {
		// If the error is related to fromTemplate, set the build status to failed with the appropriate message
		// This is to unify the error handling with fromImage errors
		if _, ok := errors.AsType[*FromTemplateError](err); !ok {
			return fmt.Errorf("failed to set template source: %w", err)
		}

		err = tm.SetStatus(
			ctx,
			buildID,
			types.BuildStatusGroupFailed,
			&templatemanagergrpc.TemplateBuildStatusReason{
				Message: err.Error(),
				Step:    new("base"),
			},
		)
		if err != nil {
			return fmt.Errorf("failed to set build status: %w", err)
		}

		return nil
	}

	_, err = client.Template.TemplateCreate(
		ctx, &templatemanagergrpc.TemplateCreateRequest{
			Template:   template,
			CacheScope: new(teamID.String()),
			Version:    &version,
		},
	)

	err = utils.UnwrapGRPCError(err)
	if err != nil {
		return fmt.Errorf("failed to create template '%s': %w", templateID, err)
	}
	telemetry.ReportEvent(ctx, "Template build started")

	// status building must be set after build is triggered because then
	// it's possible build status job will be triggered before build cache on template manager is created and build will fail
	err = tm.SetStatus(
		ctx,
		buildID,
		types.BuildStatusGroupInProgress,
		nil,
	)
	if err != nil {
		return fmt.Errorf("failed to set build status to building: %w", err)
	}
	telemetry.ReportEvent(ctx, "created new environment", telemetry.WithTemplateID(templateID))

	// Do not wait for global build sync trigger it immediately
	go func(ctx context.Context) {
		ctx, span := tracer.Start(ctx, "template-background-build-env")
		defer span.End()

		l := logger.L().With(logger.WithBuildID(buildID.String()), logger.WithTemplateID(templateID))

		err := tm.BuildStatusSync(ctx, buildID, templateID, clusterID, &nodeID)
		if err != nil {
			l.Error(ctx, "error syncing build status", zap.Error(err))
		}

		telemetry.ReportEvent(ctx, "build status sync completed")

		// Invalidate the cache
		invalidatedKeys := tm.templateCache.InvalidateAllTags(context.WithoutCancel(ctx), templateID)

		telemetry.ReportEvent(ctx, "invalidated template cache", attribute.StringSlice("invalidated_keys", invalidatedKeys))
	}(context.WithoutCancel(ctx))

	return nil
}

func convertTemplateSteps(steps *[]api.TemplateStep) []*templatemanagergrpc.TemplateStep {
	if steps == nil {
		return nil
	}

	result := make([]*templatemanagergrpc.TemplateStep, len(*steps))
	for i, step := range *steps {
		var args []string
		if step.Args != nil {
			args = *step.Args
		}

		result[i] = &templatemanagergrpc.TemplateStep{
			Type:      step.Type,
			Args:      args,
			FilesHash: step.FilesHash,
			Force:     step.Force,
		}
	}

	return result
}

func convertImageRegistry(registry *api.FromImageRegistry) (*templatemanagergrpc.FromImageRegistry, error) {
	if registry == nil {
		return nil, nil
	}

	// The OpenAPI FromImageRegistry is a union type, so we need to check the discriminator
	discriminator, err := registry.Discriminator()
	if err != nil {
		return nil, err
	}

	switch discriminator {
	case "aws":
		awsReg, err := registry.AsAWSRegistry()
		if err != nil {
			return nil, err
		}

		return &templatemanagergrpc.FromImageRegistry{
			Type: &templatemanagergrpc.FromImageRegistry_Aws{
				Aws: &templatemanagergrpc.AWSRegistry{
					AwsAccessKeyId:     awsReg.AwsAccessKeyId,
					AwsSecretAccessKey: awsReg.AwsSecretAccessKey,
					AwsRegion:          awsReg.AwsRegion,
				},
			},
		}, nil
	case "gcp":
		gcpReg, err := registry.AsGCPRegistry()
		if err != nil {
			return nil, err
		}

		return &templatemanagergrpc.FromImageRegistry{
			Type: &templatemanagergrpc.FromImageRegistry_Gcp{
				Gcp: &templatemanagergrpc.GCPRegistry{
					ServiceAccountJson: gcpReg.ServiceAccountJson,
				},
			},
		}, nil
	case "registry":
		generalReg, err := registry.AsGeneralRegistry()
		if err != nil {
			return nil, err
		}

		return &templatemanagergrpc.FromImageRegistry{
			Type: &templatemanagergrpc.FromImageRegistry_General{
				General: &templatemanagergrpc.GeneralRegistry{
					Username: generalReg.Username,
					Password: generalReg.Password,
				},
			},
		}, nil
	default:
		return nil, fmt.Errorf("unknown registry type: %s", discriminator)
	}
}

// setTemplateSource sets the source (either fromImage or fromTemplate)
func setTemplateSource(ctx context.Context, tm *TemplateManager, teamID uuid.UUID, teamSlug string, template *templatemanagergrpc.TemplateConfig, fromImage *string, fromTemplate *string) error {
	hasImage := fromImage != nil && *fromImage != ""
	hasTemplate := fromTemplate != nil && *fromTemplate != ""

	// Validate input: exactly one source must be provided
	switch {
	case hasImage && hasTemplate:
		return errors.New("cannot specify both fromImage and fromTemplate")
	case !hasImage && !hasTemplate:
		return errors.New("must specify either fromImage or fromTemplate")
	case hasTemplate:
		identifier, tag, err := id.ParseName(*fromTemplate)
		if err != nil {
			return &FromTemplateError{
				err:     err,
				message: fmt.Sprintf("invalid template reference: %s", err),
			}
		}

		// Step 1: Resolve alias to template ID (using cache with fallback for promoted templates)
		aliasInfo, metadata, err := tm.templateCache.ResolveAliasWithMetadata(ctx, identifier, teamSlug)
		if err != nil {
			apiErr := templatecache.ErrorToAPIError(err, identifier)

			return &FromTemplateError{
				err:     err,
				message: apiErr.ClientMsg,
			}
		}

		ref := templatecache.TemplateRef{
			Identifier: aliasInfo.MatchedIdentifier,
			Visible:    aliasInfo.TeamID == teamID || metadata.Public,
		}

		// Step 2: Get template with build by ID
		_, build, err := tm.templateCache.Get(ctx, aliasInfo.TemplateID, tag, teamID, metadata.ClusterID)
		if err != nil {
			apiErr := ref.APIError(err)

			return &FromTemplateError{
				err:     err,
				message: apiErr.ClientMsg,
			}
		}

		template.Source = &templatemanagergrpc.TemplateConfig_FromTemplate{
			FromTemplate: &templatemanagergrpc.FromTemplateConfig{
				Alias:   *fromTemplate,
				BuildID: build.ID.String(),
			},
		}
	default: // hasImage
		template.Source = &templatemanagergrpc.TemplateConfig_FromImage{
			FromImage: *fromImage,
		}
	}

	return nil
}
```

`packages/api/internal/template-manager/template_manager.go` lines 216-229 (of 229)

```go

func (tm *TemplateManager) GetStatus(ctx context.Context, buildID uuid.UUID, templateID string, clusterID uuid.UUID, nodeID string) (*templatemanagergrpc.TemplateBuildStatusResponse, error) {
	client, err := tm.GetClusterBuildClient(clusterID, nodeID)
	if err != nil {
		return nil, fmt.Errorf("failed to get builder client: %w", err)
	}

	// error unwrapping is done in the caller
	return client.Template.TemplateBuildStatus(
		ctx, &templatemanagergrpc.TemplateStatusRequest{
			BuildID: buildID.String(), TemplateID: templateID,
		},
	)
}
```

`packages/api/internal/template-manager/template_status.go` lines 1-422 (of 422)

```go
package template_manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/flowchartsman/retry"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/db/pkg/types"
	"github.com/e2b-dev/infra/packages/db/queries"
	templatemanagergrpc "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

var (
	buildTimeout             = time.Hour
	syncWaitingStateDeadline = time.Minute * 40
)

// terminalWriteTimeout bounds a write that records a build's final status.
const terminalWriteTimeout = 30 * time.Second

// terminalWriteContext returns the context for recording a build's final
// status. It is detached from ctx because the poll context expires at the build
// deadline, and pgx fast-fails an expired context while the retry wrapper does
// not retry context errors: a terminal write made on it never lands, leaving
// the build in progress forever.
func terminalWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), terminalWriteTimeout)
}

func (tm *TemplateManager) BuildStatusSync(ctx context.Context, buildID uuid.UUID, templateID string, clusterID uuid.UUID, nodeID *string) error {
	if tm.createInProcessingQueue(buildID, templateID) {
		// already processing, skip
		return nil
	}

	// remove from processing queue when done
	defer tm.removeFromProcessingQueue(buildID)

	result, err := tm.sqlcDB.GetTemplateBuildWithTemplate(ctx, queries.GetTemplateBuildWithTemplateParams{
		TemplateID: templateID,
		BuildID:    buildID,
	})
	if err != nil {
		return fmt.Errorf("failed to get env build: %w", err)
	}

	envBuild := result.EnvBuild
	// waiting for build to start, local docker build and push can take some time
	// so just check if it's not too long
	if envBuild.StatusGroup == types.BuildStatusGroupPending {
		// if waiting for too long, fail the build
		if time.Since(envBuild.CreatedAt) > syncWaitingStateDeadline {
			err = tm.SetStatus(ctx, buildID, types.BuildStatusGroupFailed, &templatemanagergrpc.TemplateBuildStatusReason{
				Message: "build is in waiting state for too long",
			})
			if err != nil {
				logger.L().Error(ctx, "error when setting build status to failed after waiting for too long", zap.Error(err), logger.WithBuildID(buildID.String()), logger.WithTemplateID(templateID))
			}

			return errors.New("build is in waiting state for too long, failing it")
		}

		// just wait for next sync
		return nil
	}

	if nodeID == nil {
		return errors.New("build is not assigned to a node, but it should be")
	}

	checker := &PollBuildStatus{
		client: tm,
		logger: logger.L().With(logger.WithBuildID(buildID.String()), logger.WithTemplateID(templateID)),

		templateID: templateID,
		buildID:    buildID,

		clusterID: clusterID,
		nodeID:    *nodeID,
	}

	// context for the building phase
	ctx, buildCancel := context.WithTimeout(ctx, buildTimeout)
	defer buildCancel()

	checker.poll(ctx)

	return nil
}

type templateManagerClient interface {
	SetTerminalStatus(ctx context.Context, buildID uuid.UUID, statusGroup types.BuildStatusGroup, reason *templatemanagergrpc.TemplateBuildStatusReason) (bool, error)
	SetFinished(ctx context.Context, buildID uuid.UUID, rootfsSize int64, envdVersion, kernelVersion, firecrackerVersion string) error
	GetStatus(ctx context.Context, buildId uuid.UUID, templateID string, clusterID uuid.UUID, nodeID string) (*templatemanagergrpc.TemplateBuildStatusResponse, error)
	DeleteBuild(ctx context.Context, buildID uuid.UUID, templateID string, clusterID uuid.UUID, nodeID string) error
}

type PollBuildStatus struct {
	logger logger.Logger
	client templateManagerClient

	templateID string
	buildID    uuid.UUID

	clusterID uuid.UUID
	nodeID    string

	status *templatemanagergrpc.TemplateBuildStatusResponse
}

func (c *PollBuildStatus) poll(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// A cancelled window means the poller is going away, not the build.
			// The periodical sync re-adopts the build, so nothing is recorded.
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				c.logger.Debug(ctx, "Build status polling stopped, leaving the build in progress", zap.Error(ctx.Err()))

				return
			}

			c.logger.Debug(ctx, "Build status polling timed out, stopping polling")

			if c.setFailed(ctx, fmt.Sprintf("build status polling timed out. Maximum build time is %s.", buildTimeout)) {
				c.cancelBuildOnNode(ctx)
			}

			return
		case <-ticker.C:
			buildCompleted, err := c.checkBuildStatus(ctx)
			if err != nil {
				// The check was cut short by the window closing, which says nothing
				// about the build. The ctx.Done() case decides what to record.
				if ctx.Err() != nil {
					c.logger.Debug(ctx, "Build status polling window closed mid-check",
						zap.Error(errors.Join(ctx.Err(), err)))

					continue
				}

				c.logger.Error(ctx, "Build status polling received unrecoverable error", zap.Error(err))

				c.setFailed(ctx, fmt.Sprintf("polling received unrecoverable error: %s", err))

				return
			}

			// build status can return empty error when build is still in progress
			// this will cause fast return to avoid pooling when build is already finished
			if buildCompleted {
				return
			}
		}
	}
}

// setFailed records the build as failed. It reports false when another poller
// ended the build first, in which case this write changed nothing.
func (c *PollBuildStatus) setFailed(ctx context.Context, message string) bool {
	writeCtx, cancel := terminalWriteContext(ctx)
	defer cancel()

	recorded, err := c.client.SetTerminalStatus(writeCtx, c.buildID, types.BuildStatusGroupFailed, &templatemanagergrpc.TemplateBuildStatusReason{
		Message: message,
	})
	if err != nil {
		c.logger.Error(writeCtx, "error when setting build status", zap.Error(err))
	}

	return recorded
}

// cancelBuildOnNode stops a build the poller has just failed for running out of
// time. The builder has no deadline of its own, and a build that is no longer in
// progress is watched by neither the periodical sync nor the admin cancel
// endpoint, so left alone it holds its node slot to completion. The delete takes
// the build's artifacts with it, so it must follow a write that ended the build
// rather than one that lost to another poller.
func (c *PollBuildStatus) cancelBuildOnNode(ctx context.Context) {
	writeCtx, cancel := terminalWriteContext(ctx)
	defer cancel()

	err := c.client.DeleteBuild(writeCtx, c.buildID, c.templateID, c.clusterID, c.nodeID)
	if err != nil {
		c.logger.Error(writeCtx, "error when cancelling the timed-out build on the node", zap.Error(err))
	}
}

// terminalError is a terminal error that should not be retried
// set like this so that we can check for it using errors.As
type terminalError struct {
	err error
}

func (e terminalError) Error() string {
	return e.err.Error()
}

func newTerminalError(err error) error {
	return terminalError{
		err: retry.Stop(err),
	}
}

func (c *PollBuildStatus) setStatus(ctx context.Context) error {
	status, err := c.client.GetStatus(ctx, c.buildID, c.templateID, c.clusterID, c.nodeID)
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("context deadline exceeded: %w", err)
	} else if err != nil { // retry only on context deadline exceeded
		c.logger.Error(ctx, "terminal error when polling build status", zap.Error(err))

		return newTerminalError(err)
	}

	if status == nil {
		return errors.New("nil status") // this should never happen
	}

	// debug log the status
	c.logger.Debug(ctx, "setting status pointer", zap.String("status", status.GetStatus().String()))

	c.status = status

	return nil
}

func (c *PollBuildStatus) dispatchBasedOnStatus(ctx context.Context, status *templatemanagergrpc.TemplateBuildStatusResponse) (bool, error) {
	if status == nil {
		return false, errors.New("nil status")
	}
	switch status.GetStatus() {
	case templatemanagergrpc.TemplateBuildState_Failed:
		// build failed
		writeCtx, cancel := terminalWriteContext(ctx)
		defer cancel()

		_, err := c.client.SetTerminalStatus(writeCtx, c.buildID, types.BuildStatusGroupFailed, status.GetReason())
		if err != nil {
			return false, fmt.Errorf("error when setting build status: %w", err)
		}

		return true, nil
	case templatemanagergrpc.TemplateBuildState_Completed:
		// build completed
		meta := status.GetMetadata()
		if meta == nil {
			return false, errors.New("nil metadata")
		}

		writeCtx, cancel := terminalWriteContext(ctx)
		defer cancel()

		err := c.client.SetFinished(
			writeCtx,
			c.buildID,
			int64(meta.GetRootfsSizeKey()),
			meta.GetEnvdVersionKey(),
			meta.GetKernelVersion(),
			meta.GetFirecrackerVersion(),
		)
		if err != nil {
			return false, fmt.Errorf("error when finishing build: %w", err)
		}

		return true, nil
	default:
		c.logger.Debug(ctx, "skipping status", zap.String("status", status.GetStatus().String()))

		return false, nil
	}
}

func (c *PollBuildStatus) checkBuildStatus(ctx context.Context) (bool, error) {
	c.logger.Debug(ctx, "Checking template build status")

	retrier := retry.NewRetrier(
		10,
		100*time.Millisecond,
		time.Second,
	)

	err := retrier.RunContext(ctx, c.setStatus)
	if err != nil {
		c.logger.Error(ctx, "error when calling setStatus", zap.Error(err))

		return false, err
	}

	c.logger.Debug(ctx, "dispatching based on status", zap.String("status", c.status.GetStatus().String()))

	buildCompleted, err := c.dispatchBasedOnStatus(ctx, c.status)
	if err != nil {
		return false, fmt.Errorf("error when dispatching build status: %w", err)
	}

	return buildCompleted, nil
}

func (tm *TemplateManager) removeFromProcessingQueue(buildID uuid.UUID) {
	tm.lock.Lock()
	delete(tm.processing, buildID)
	tm.lock.Unlock()
}

func (tm *TemplateManager) createInProcessingQueue(buildID uuid.UUID, templateID string) bool {
	tm.lock.Lock()
	defer tm.lock.Unlock()

	_, exists := tm.processing[buildID]
	if exists {
		// already in processing queue, skip
		return true
	}

	tm.processing[buildID] = processingBuilds{templateID: templateID}

	return false
}

func (tm *TemplateManager) SetStatus(ctx context.Context, buildID uuid.UUID, statusGroup types.BuildStatusGroup, reason *templatemanagergrpc.TemplateBuildStatusReason) error {
	if statusGroup.IsTerminal() {
		_, err := tm.SetTerminalStatus(ctx, buildID, statusGroup, reason)

		return err
	}

	now := time.Now()

	err := tm.sqlcDB.UpdateEnvBuildStatus(ctx, queries.UpdateEnvBuildStatusParams{
		Status:     buildStatus(statusGroup),
		FinishedAt: &now,
		Reason:     buildReasonFrom(reason),
		BuildID:    buildID,
	})

	tm.buildCache.Invalidate(ctx, buildID)

	return err
}

// SetTerminalStatus records a build's final outcome and reports whether this
// write is the one that ended it: several pollers watch the same build, and the
// build keeps the first outcome recorded.
func (tm *TemplateManager) SetTerminalStatus(ctx context.Context, buildID uuid.UUID, statusGroup types.BuildStatusGroup, reason *templatemanagergrpc.TemplateBuildStatusReason) (bool, error) {
	buildReason := buildReasonFrom(reason)

	logger.L().Warn(ctx, "Setting template build status to terminal failure",
		logger.WithBuildID(buildID.String()),
		zap.String("reason", buildReason.Message),
	)

	now := time.Now()

	recorded, err := tm.sqlcDB.FailTemplateBuildAndDeactivate(ctx, queries.FailTemplateBuildAndDeactivateParams{
		Status:     buildStatus(statusGroup),
		FinishedAt: &now,
		Reason:     buildReason,
		BuildID:    buildID,
	})

	tm.buildCache.Invalidate(ctx, buildID)

	return recorded, err
}

func buildReasonFrom(reason *templatemanagergrpc.TemplateBuildStatusReason) types.BuildReason {
	if reason == nil {
		return types.BuildReason{}
	}

	buildReason := types.BuildReason{
		Message: reason.GetMessage(),
	}
	if step := reason.GetStep(); step != "" {
		buildReason.Step = &step
	}

	return buildReason
}

func (tm *TemplateManager) SetFinished(ctx context.Context, buildID uuid.UUID, rootfsSize int64, envdVersion, kernelVersion, firecrackerVersion string) error {
	// first do database update to prevent race condition while calling status
	// TODO(ENG-3469): Switch to types.BuildStatusReady once all consumers are migrated.
	err := tm.sqlcDB.FinishTemplateBuild(ctx, queries.FinishTemplateBuildParams{
		TotalDiskSizeMb:    &rootfsSize,
		Status:             types.BuildStatusUploaded,
		EnvdVersion:        &envdVersion,
		KernelVersion:      kernelVersion,
		FirecrackerVersion: firecrackerVersion,
		BuildID:            buildID,
	})

	tm.buildCache.Invalidate(ctx, buildID)

	return err
}

// buildStatus maps a status group to a default build status for the database.
func buildStatus(g types.BuildStatusGroup) types.BuildStatus {
	switch g {
	case types.BuildStatusGroupPending:
		return types.BuildStatusPending
	case types.BuildStatusGroupInProgress:
		return types.BuildStatusBuilding
	case types.BuildStatusGroupReady:
		return types.BuildStatusUploaded
	case types.BuildStatusGroupFailed:
		return types.BuildStatusFailed
	default:
		return types.BuildStatusFailed
	}
}
```

Caller (`POST /v2/templates/{id}/builds/{buildID}` start-build handler): `diskSizeMB` is the team's
`DiskMb` limit (build-time working space), `freeDiskSizeMB` is the per-build free-space target;
`version` is the template-builder version derived from the SDK User-Agent (default `v2.1.0`):

`packages/api/internal/handlers/template_start_build_v2.go` lines 211-270 (of 328)

```go
	version, err := userAgentToTemplateVersion(ctx, logger.L().With(logger.WithTemplateID(templateID), logger.WithBuildID(buildID)), c.Request.UserAgent())
	if err != nil {
		a.sendAPIStoreError(c, http.StatusBadRequest, fmt.Sprintf("Error when parsing user agent: %s", err))
		telemetry.ReportErrorByCode(ctx, http.StatusBadRequest, "error when parsing user agent", err, telemetry.WithTemplateID(templateID))

		return
	}

	builderNode, err := a.templateManager.GetAvailableBuildClient(ctx, clusterID)
	if err != nil {
		a.sendAPIStoreError(c, http.StatusServiceUnavailable, "Error when getting available build client")
		telemetry.ReportCriticalError(ctx, "error when getting available build client", err, telemetry.WithTemplateID(templateID))

		return
	}

	machineInfo := builderNode.GetMachineInfo()
	err = a.sqlcDB.UpdateTemplateBuild(ctx, queries.UpdateTemplateBuildParams{
		StartCmd:        body.StartCmd,
		ReadyCmd:        body.ReadyCmd,
		Dockerfile:      new(string(stepsMarshalled)),
		ClusterNodeID:   new(builderNode.NodeID),
		CpuArchitecture: new(machineInfo.CPUArchitecture),
		CpuFamily:       new(machineInfo.CPUFamily),
		CpuModel:        new(machineInfo.CPUModel),
		CpuModelName:    new(machineInfo.CPUModelName),
		CpuFlags:        machineInfo.CPUFlags,
		BuildUuid:       buildUUID,
	})
	if err != nil {
		telemetry.ReportCriticalError(ctx, "error when updating build", err)
		a.sendAPIStoreError(c, http.StatusInternalServerError, fmt.Sprintf("Error when updating build: %s", err))

		return
	}

	// Call the Template Manager to build the environment
	err = a.templateManager.CreateTemplate(
		ctx,
		team.ID,
		team.Slug,
		templateID,
		buildUUID,
		build.KernelVersion,
		build.FirecrackerVersion,
		body.StartCmd,
		build.Vcpu,
		team.Limits.DiskMb,   // Build-time working space available before customer steps run.
		build.FreeDiskSizeMb, // Customer's free-space target after customer steps run.
		build.RamMb,
		body.ReadyCmd,
		body.FromImage,
		body.FromTemplate,
		body.FromImageRegistry,
		body.Force,
		body.Steps,
		clusterID,
		builderNode.NodeID,
		version,
	)
```

`packages/api/internal/handlers/template_start_build_v2.go` lines 291-328 (of 328)

```go
func userAgentToTemplateVersion(ctx context.Context, logger logger.Logger, userAgent string) (string, error) {
	version := templates.TemplateV2LatestVersion

	for agent := range strings.FieldsSeq(userAgent) {
		switch {
		case strings.HasPrefix(agent, jsSDKPrefix):
			sdk := strings.TrimPrefix(agent, jsSDKPrefix)

			// Check if the SDK version supports the latest template version
			ok, err := utils.IsGTEVersion(sdk, templates.SDKTemplateReleaseVersion)
			if err != nil {
				return "", fmt.Errorf("parsing JS SDK version: %w", err)
			}
			if !ok {
				version = templates.TemplateV2BetaVersion
			}

			return version, nil
		case strings.HasPrefix(agent, pythonSDKPrefix):
			sdk := strings.TrimPrefix(agent, pythonSDKPrefix)

			// Check if the SDK version supports the latest template version
			ok, err := utils.IsGTEVersion(sdk, templates.SDKTemplateReleaseVersion)
			if err != nil {
				return "", fmt.Errorf("parsing Python SDK version: %w", err)
			}
			if !ok {
				version = templates.TemplateV2BetaVersion
			}

			return version, nil
		}
	}

	logger.Debug(ctx, "Unrecognized user agent, defaulting to the latest template version", zap.String("user_agent", userAgent), zap.String("version", version))

	return version, nil
}
```

`packages/shared/pkg/templates/versions.go` lines 1-12 (of 12)

```go
package templates

const (
	TemplateV2LatestVersion = "v2.1.0"

	TemplateV2ReleaseVersion = "v2.1.0"
	TemplateV2BetaVersion    = "v2.0.0"
)

const (
	SDKTemplateReleaseVersion = "2.3.0"
)
```

Build registration (build ID generation, CPU/RAM validation, free-disk resolution, the seeded kernel/FC versions):

`packages/api/internal/template/register_build.go` lines 84-119 (of 489)

```go
	// On the span before the free-disk resolution below can return, so a refused
	// registration still says whose request was refused.
	telemetry.SetAttributes(ctx,
		telemetry.WithTeamID(data.Team.ID.String()),
		telemetry.WithTemplateID(data.TemplateID),
	)

	// Ahead of the concurrency check and the transaction, so a target this team
	// may not request is refused as the client's error either way, and the value
	// that reaches the row is the allowance as it stood at registration.
	freeDiskSizeMB, apiError := team.LimitFreeDiskSize(data.Team.Limits, data.MinFreeDiskMb)

	freeDiskAttrs := []attribute.KeyValue{
		attribute.Int64("build.free_disk.default_mb", data.Team.Limits.DefaultFreeDiskSizeMb),
		attribute.Int64("build.free_disk.max_mb", data.Team.Limits.MaxFreeDiskSizeMb),
	}
	if data.MinFreeDiskMb != nil {
		freeDiskAttrs = append(freeDiskAttrs,
			attribute.Int64("build.free_disk.requested_mb", int64(*data.MinFreeDiskMb)))
	}
	telemetry.SetAttributes(ctx, freeDiskAttrs...)

	if apiError != nil {
		telemetry.ReportErrorByCode(ctx, apiError.Code, "free disk space request refused", apiError.Err,
			telemetry.WithTeamID(data.Team.ID.String()))

		return nil, apiError
	}

	telemetry.SetAttributes(ctx, attribute.Int64("build.free_disk.mb", freeDiskSizeMB))

	// Add default tag if no tags are present
	tags := data.Tags
	if len(tags) == 0 {
		tags = []string{id.DefaultTag}
	}
```

`packages/api/internal/template/register_build.go` lines 149-159 (of 489)

```go
	// Generate a build id for the new build
	buildID, err := uuid.NewRandom()
	if err != nil {
		telemetry.ReportCriticalError(ctx, "error when generating build id", err)

		return nil, &api.APIError{
			Err:       err,
			ClientMsg: "Failed to generate build id",
			Code:      http.StatusInternalServerError,
		}
	}
```

`packages/api/internal/template/register_build.go` lines 192-195 (of 489)

```go
	cpuCount, ramMB, apiError := team.LimitResources(data.Team.Limits, data.CpuCount, data.MemoryMB)
	if apiError != nil {
		return nil, apiError
	}
```

`packages/api/internal/template/register_build.go` lines 283-300 (of 489)

```go
	// Insert the new build
	// TODO(ENG-3469): Switch to dbtypes.BuildStatusPending once all consumers are migrated.
	// kernel_version and firecracker_version are seeded here for backwards
	// compatibility; the template-manager reports the versions it actually
	// used in TemplateBuildMetadata and SetFinished overwrites these rows.
	err = client.CreateTemplateBuild(ctx, queries.CreateTemplateBuildParams{
		BuildID:            buildID,
		Status:             dbtypes.BuildStatusWaiting,
		RamMb:              ramMB,
		Vcpu:               cpuCount,
		KernelVersion:      data.KernelVersion,
		FirecrackerVersion: data.FirecrackerVersion,
		FreeDiskSizeMb:     freeDiskSizeMB,
		StartCmd:           data.StartCmd,
		ReadyCmd:           data.ReadyCmd,
		Dockerfile:         new(data.Dockerfile),
		Version:            new(data.Version),
	})
```

`packages/api/internal/handlers/template_request_build_v3.go` lines 100-106 (of 189)

```go
	}
	clusterID := clusters.WithClusterFallback(currentCluster)
	findTemplateCtx, span := tracer.Start(ctx, "find-template-alias")
	defer span.End()
	templateID := id.Generate()
	public := false
	replacesTemplateID := ""
```

`packages/api/internal/handlers/template_request_build_v3.go` lines 128-152 (of 189)

```go
	}
	span.End()

	// TODO(ENG-3852): Stop sending the firecracker/kernel versions from the API.
	// The orchestrator resolves them itself via the BuildFirecrackerVersion /
	// BuildKernelVersion feature flags (see packages/orchestrator/pkg/template/server/create_template.go).
	firecrackerVersion := a.featureFlags.StringFlag(ctx, featureflags.BuildFirecrackerVersion)
	kernelVersion := a.featureFlags.StringFlag(ctx, featureflags.BuildKernelVersion)
	buildReq := template.RegisterBuildData{
		ReplacesTemplateID: replacesTemplateID,
		Public:             public,
		ClusterID:          clusterID,
		TemplateID:         templateID,
		UserID:             nil,
		Team:               team,
		Alias:              &identifier,
		Tags:               tags,
		CpuCount:           body.CpuCount,
		MemoryMB:           body.MemoryMB,
		MinFreeDiskMb:      body.MinFreeDiskMb,
		Version:            templates.TemplateV2LatestVersion,
		KernelVersion:      kernelVersion,
		FirecrackerVersion: firecrackerVersion,
	}

```

---

## 4. How E2B calls envd

### 4.1 Worked example through the orchestrator proxy: `packages/orchestrator/pkg/template/build/sandboxtools/command.go` (complete)

Every template-build command (RUN steps, start command, ready command) goes through this: Connect
`Process.Start` with `/bin/bash -l -c <command>`, sent to `http://localhost<proxyAddr>` (i.e.
`http://localhost:5007`), with the sandbox addressed via the Host header and the user via Basic auth.
No `X-Access-Token` is sent here because build sandboxes are created without an envd access token.

`packages/orchestrator/pkg/template/build/sandboxtools/command.go` lines 1-261 (of 261)

```go
//go:build linux

package sandboxtools

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap/zapcore"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/proxy"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/core/rootfs"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process/processconnect"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

const commandHardTimeout = 1 * time.Hour

func RunCommandWithOutput(
	ctx context.Context,
	proxy *proxy.SandboxProxy,
	sandboxID string,
	command string,
	metadata metadata.Context,
	processOutput func(stdout, stderr string),
) error {
	return runCommandWithAllOptions(
		ctx,
		proxy,
		sandboxID,
		command,
		metadata,
		// No confirmation needed for this command
		make(chan struct{}),
		processOutput,
	)
}

func RunCommand(
	ctx context.Context,
	proxy *proxy.SandboxProxy,
	sandboxID string,
	command string,
	metadata metadata.Context,
) error {
	return runCommandWithAllOptions(
		ctx,
		proxy,
		sandboxID,
		command,
		metadata,
		// No confirmation needed for this command
		make(chan struct{}),
		func(_, _ string) {},
	)
}

func RunCommandWithLogger(
	ctx context.Context,
	proxy *proxy.SandboxProxy,
	logger logger.Logger,
	lvl zapcore.Level,
	id string,
	sandboxID string,
	command string,
	metadata metadata.Context,
) error {
	return RunCommandWithConfirmation(
		ctx,
		proxy,
		logger,
		lvl,
		id,
		sandboxID,
		command,
		metadata,
		// No confirmation needed for this command
		make(chan struct{}),
	)
}

func RunCommandWithConfirmation(
	ctx context.Context,
	proxy *proxy.SandboxProxy,
	logger logger.Logger,
	lvl zapcore.Level,
	id string,
	sandboxID string,
	command string,
	metadata metadata.Context,
	confirmCh chan<- struct{},
) error {
	return runCommandWithAllOptions(
		ctx,
		proxy,
		sandboxID,
		command,
		metadata,
		confirmCh,
		func(stdout, stderr string) {
			logStream(ctx, logger, lvl, id, "stdout", stdout)
			logStream(ctx, logger, lvl, id, "stderr", stderr)
		},
	)
}

func runCommandWithAllOptions(
	ctx context.Context,
	proxy *proxy.SandboxProxy,
	sandboxID string,
	command string,
	metadata metadata.Context,
	confirmCh chan<- struct{},
	processOutput func(stdout, stderr string),
) (e error) {
	ctx, span := tracer.Start(ctx, "run command", trace.WithAttributes(attribute.String("command", command), telemetry.WithSandboxID(sandboxID)))
	defer span.End()
	defer func() {
		if e != nil {
			span.RecordError(e)
			span.SetStatus(codes.Error, e.Error())
		}
	}()

	// Append standard directories to PATH so that utilities are always
	// findable even if the user sets PATH to something broken.
	envs := maps.Clone(metadata.EnvVars)
	if _, ok := envs["PATH"]; ok {
		envs["PATH"] += ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}

	runCmdReq := connect.NewRequest(&process.StartRequest{
		Process: &process.ProcessConfig{
			Cmd: "/bin/bash",
			Cwd: metadata.WorkDir,
			Args: []string{
				"-l", "-c", command,
			},
			Envs: envs,
		},
	})

	hc := http.Client{
		Timeout:   commandHardTimeout,
		Transport: sandbox.SandboxHttpTransport,
	}

	proxyHost := fmt.Sprintf("http://localhost%s", proxy.GetAddr())
	processC := processconnect.NewProcessClient(&hc, proxyHost)
	err := grpc.SetSandboxHeader(runCmdReq.Header(), proxyHost, sandboxID)
	if err != nil {
		return fmt.Errorf("failed to set sandbox header: %w", err)
	}
	grpc.SetUserHeader(runCmdReq.Header(), metadata.User)

	processCtx, processCancel := context.WithCancel(ctx)
	defer processCancel()
	commandStream, err := processC.Start(processCtx, runCmdReq)
	// Confirm the command has executed before proceeding
	close(confirmCh)
	if err != nil {
		return fmt.Errorf("error starting process: %w", err)
	}
	defer func() {
		processCancel()
		commandStream.Close()
	}()

	msgCh, msgErrCh := grpc.StreamToChannel(ctx, commandStream)

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context: %w", ctx.Err())
		case err := <-msgErrCh:
			return fmt.Errorf("command failed: %w", err)
		case msg, ok := <-msgCh:
			if !ok {
				// The terminal status, if any, is in msgErrCh before msgCh
				// closes - drain it so a failure is not reported as success.
				select {
				case err := <-msgErrCh:
					return fmt.Errorf("command failed: %w", err)
				default:
					return nil
				}
			}
			e := msg.GetEvent()
			if e == nil {
				logger.L().Error(ctx, "received nil command event")

				return nil
			}

			switch {
			case e.GetData() != nil:
				data := e.GetData()
				processOutput(string(data.GetStdout()), string(data.GetStderr()))

			case e.GetEnd() != nil:
				end := e.GetEnd()
				success := end.GetExitCode() == 0

				if !success {
					return errors.New(end.GetStatus())
				}
			}
		}
	}
}

func logStream(ctx context.Context, logger logger.Logger, lvl zapcore.Level, id string, name string, content string) {
	if logger == nil {
		return
	}

	if content == "" {
		return
	}
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		msg := fmt.Sprintf("[%s] [%s]: %s", id, name, line)
		logger.Log(ctx, lvl, msg)
	}
}

// syncChangesToDisk synchronizes filesystem changes to the filesystem
// This is useful to ensure that all changes made in the sandbox are written to disk
// to be able to re-create the sandbox without resume.
func SyncChangesToDisk(
	ctx context.Context,
	proxy *proxy.SandboxProxy,
	sandboxID string,
) error {
	return RunCommand(
		ctx,
		proxy,
		sandboxID,
		rootfs.SandboxBusyBoxPath+" sync",
		metadata.Context{
			User: "root",
		},
	)
}
```

The header helpers (`packages/shared/pkg/grpc/envd_command.go`, complete). `SetSandboxHeader` sets
`Host: 49983-<sandboxID>-00000000.<domain of the base URL>`; `SetUserHeader` sets
`Authorization: Basic base64("<user>:")` (empty password).

`packages/shared/pkg/grpc/envd_command.go` lines 1-76 (of 76)

```go
package grpc

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"

	"connectrpc.com/connect"

	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
)

// StreamToChannel pumps messages from stream into the returned message
// channel. The error channel holds the terminal status of the stream (stream
// error or ctx cancellation), if any, before the message channel is closed:
// after observing the closed message channel, callers must drain the error
// channel to learn how the stream ended.
func StreamToChannel[Res any](ctx context.Context, stream *connect.ServerStreamForClient[Res]) (<-chan *Res, <-chan error) {
	out := make(chan *Res)
	// Buffered so the terminal send never blocks.
	errCh := make(chan error, 1)

	go func() {
		defer close(out)

		for stream.Receive() {
			select {
			case <-ctx.Done():
				errCh <- ctx.Err()

				return
			case out <- stream.Msg():
				// Send the message to the channel
			}
		}

		if err := stream.Err(); err != nil {
			errCh <- err

			return
		}
	}()

	return out, errCh
}

func SetSandboxHeader(header http.Header, hostname string, sandboxID string) error {
	domain, err := extractDomain(hostname)
	if err != nil {
		return fmt.Errorf("failed to extract domain from hostname: %w", err)
	}
	// Construct the host (<port>-<sandbox id>-<old client id>.e2b.app)
	host := fmt.Sprintf("%d-%s-00000000.%s", consts.DefaultEnvdServerPort, sandboxID, domain)

	header.Set("Host", host)

	return nil
}

func SetUserHeader(header http.Header, user string) {
	userString := fmt.Sprintf("%s:", user)
	userBase64 := base64.StdEncoding.EncodeToString([]byte(userString))
	basic := fmt.Sprintf("Basic %s", userBase64)
	header.Set("Authorization", basic)
}

func extractDomain(input string) (string, error) {
	parsedURL, err := url.Parse(input)
	if err != nil || parsedURL.Host == "" {
		return "", fmt.Errorf("invalid URL: %s", input)
	}

	return parsedURL.Hostname(), nil
}
```

`packages/shared/pkg/consts/envd.go` lines 1-20 (of 20)

```go
package consts

const (
	DefaultEnvdServerPort int64 = 49983

	// SystemTag opts a process into envd's root cgroup (no user/pty/socat).
	// Used for maintenance commands that must outlive cgroup freezing.
	SystemTag = "_system"

	// TemplateDefaultUser is the user a template build makes envd's default.
	//
	// One definition, because two independent places have to agree on it and the
	// agreement is load-bearing: the finalize phase sends this literal for any build
	// below templates.TemplateV2ReleaseVersion and the recorded Context.User at or
	// above it, so this is the only value BOTH branches produce. A host reconstructing
	// what a build sent can therefore trust exactly this value and nothing else. Two
	// copies that drift would make the reconstruction re-send a user finalize never
	// sent.
	TemplateDefaultUser = "user"
)
```

(Commentary, verified against connect-go source) Go's `net/http` ignores a `Host` entry in
`Request.Header`; connect-go promotes it to `Request.Host` before sending, which is why the
`header.Set("Host", ...)` above works for Connect calls. The repo pins `connectrpc.com/connect v1.18.1`;
the snippet below is from v1.20.0 in the local module cache. For plain `net/http` requests the
integration tests set `req.Host` explicitly (§4.3). Simplest alternative for a client: target the
proxy by IP and send `E2b-Sandbox-Id` / `E2b-Sandbox-Port` headers (§5.1).

`connectrpc.com/connect@v1.20.0/duplex_http_call.go` lines 1-12

```go

func (d *duplexHTTPCall) makeRequest() {
	// This runs concurrently with Write and CloseWrite. Read and CloseRead wait
	// on d.responseReady, so we can't race with them.
	defer close(d.responseReady)

	// Promote the header Host to the request object.
	if host := getHeaderCanonical(d.request.Header, headerHost); len(host) > 0 {
		d.request.Host = host
	}
	if d.onRequestSend != nil {
		d.onRequestSend(d.request)
```
(the snippet above is lines 295-306 of that file)

HTTP transport used for sandbox traffic (no keep-alives, HTTP/1.1):

`packages/orchestrator/pkg/sandbox/sandbox.go` lines 105-118 (of 4288)

```go
var ErrFcProcessExited = errors.New("fc process exited prematurely")

var SandboxHttpTransport = otelhttp.NewTransport(
	&http.Transport{
		DisableKeepAlives: true,
		ForceAttemptHTTP2: false,
	},
)

// Http client that should be used for requests to sandboxes.
var sandboxHttpClient = http.Client{
	Timeout:   10 * time.Second,
	Transport: SandboxHttpTransport,
}
```

### 4.2 Interactive PTY: start, input stream, resize (`packages/orchestrator/cmd/resume-build/shell.go`, complete)

This debug tool talks to envd directly at the slot IP (`http://<slotIP>:49983`), not through the
proxy; through the proxy the only difference is the base URL plus routing headers. It shows the exact
message shapes: `StartRequest{Process, Pty{Size{Cols,Rows}}}`, wait for `StartEvent.pid`, then a client
stream `StreamInput` whose first message is `StartEvent{ProcessSelector{pid}}` followed by
`DataEvent{ProcessInput{pty: bytes}}`, and `Update{ProcessSelector, Pty{Size}}` for resize; each call
carries `Authorization: Basic` (user) and `X-Access-Token` when the sandbox has one.

`packages/orchestrator/cmd/resume-build/shell.go` lines 1-292 (of 292)

```go
//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/term"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process/processconnect"
)

// shellExitedError is returned when the in-guest shell exits cleanly
// (e.g. user pressed Ctrl+D). Callers use it to distinguish a normal
// session end from a real transport/setup error.
type shellExitedError struct{ exitCode int32 }

func (e *shellExitedError) Error() string {
	return fmt.Sprintf("shell exited with code %d", e.exitCode)
}

func isShellExited(err error) bool {
	var s *shellExitedError

	return errors.As(err, &s)
}

// shellEnv builds the environment passed into the in-guest PTY shell.
// envd intentionally only inherits PATH/HOME/USER/LOGNAME plus its own
// configured globals, so we must propagate TERM (and a few common locale
// vars) explicitly — otherwise curses apps like htop, tmux, vim and less
// fail to initialise.
func shellEnv() map[string]string {
	envs := map[string]string{}

	if t := os.Getenv("TERM"); t != "" {
		envs["TERM"] = t
	} else {
		envs["TERM"] = "xterm-256color"
	}

	for _, k := range []string{"LANG", "LC_ALL", "LC_CTYPE", "COLORTERM"} {
		if v := os.Getenv(k); v != "" {
			envs[k] = v
		}
	}

	return envs
}

// attachShell opens an interactive PTY shell against envd inside sbx,
// proxying the host terminal through. The shell is /bin/bash -l; if
// bash is missing in the guest the wrapper's stderr ("nice: '/bin/bash':
// No such file or directory") will surface in the user's terminal.
//
// Returns when the in-guest shell exits (Ctrl+D), or when ctx is cancelled.
func attachShell(ctx context.Context, sbx *sandbox.Sandbox) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("-shell requires an interactive terminal on stdin")
	}

	envdURL := fmt.Sprintf("http://%s:%d", sbx.Slot.HostIPString(), consts.DefaultEnvdServerPort)
	hc := http.Client{
		// No request timeout — interactive sessions can be long-lived.
		Transport: sandbox.SandboxHttpTransport,
	}
	processC := processconnect.NewProcessClient(&hc, envdURL)

	cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || cols == 0 || rows == 0 {
		cols, rows = 80, 24
	}

	startReq := connect.NewRequest(&process.StartRequest{
		Process: &process.ProcessConfig{
			Cmd:  "/bin/bash",
			Args: []string{"-l"},
			Envs: shellEnv(),
		},
		Pty: &process.PTY{
			Size: &process.PTY_Size{Cols: uint32(cols), Rows: uint32(rows)},
		},
	})
	grpc.SetUserHeader(startReq.Header(), "root")
	if sbx.Config.Envd.AccessToken != nil {
		startReq.Header().Set("X-Access-Token", *sbx.Config.Envd.AccessToken)
	}

	stream, err := processC.Start(ctx, startReq)
	if err != nil {
		return fmt.Errorf("start shell: %w", err)
	}
	closeStream := sync.OnceFunc(func() { _ = stream.Close() })
	defer closeStream()

	// Wait for the StartEvent so we have a pid to address input/resize at.
	var pid uint32
	for stream.Receive() {
		event := stream.Msg().GetEvent().GetEvent()
		switch e := event.(type) {
		case *process.ProcessEvent_Start:
			pid = e.Start.GetPid()
		case *process.ProcessEvent_Data:
			// Push any data that arrived before we exited the bootstrap loop.
			if pty := e.Data.GetPty(); pty != nil {
				_, _ = os.Stdout.Write(pty)
			}
		case *process.ProcessEvent_End:
			return endToError(e.End)
		}
		if pid != 0 {
			break
		}
	}
	if pid == 0 {
		if err := stream.Err(); err != nil {
			return fmt.Errorf("stream closed before start: %w", err)
		}

		return errors.New("no start event received")
	}

	fmt.Println("📟 Attaching shell via envd (Ctrl+D to exit)")

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	streamDone := make(chan struct{})
	endCh := make(chan *process.ProcessEvent_EndEvent, 1)
	go func() {
		defer close(streamDone)
		defer cancel()
		for stream.Receive() {
			event := stream.Msg().GetEvent().GetEvent()
			switch e := event.(type) {
			case *process.ProcessEvent_Data:
				if pty := e.Data.GetPty(); pty != nil {
					_, _ = os.Stdout.Write(pty)
				}
			case *process.ProcessEvent_End:
				endCh <- e.End

				return
			}
		}
	}()

	// Input pump: stdin → StreamInput as PTY bytes.
	go pumpInput(sessionCtx, processC, sbx, pid)

	// Resize: forward SIGWINCH to envd via Update.
	go pumpResize(sessionCtx, processC, sbx, pid)

	<-sessionCtx.Done()

	// Drain the output goroutine before restoring the terminal — otherwise
	// late PTY bytes land on a cooked-mode terminal and render stairstepped.
	closeStream()
	<-streamDone

	_ = term.Restore(int(os.Stdin.Fd()), oldState)
	fmt.Println()

	select {
	case end := <-endCh:
		return endToError(end)
	default:
	}

	if err := stream.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("shell stream: %w", err)
	}

	return nil
}

func pumpInput(
	ctx context.Context,
	processC processconnect.ProcessClient,
	sbx *sandbox.Sandbox,
	pid uint32,
) {
	in := processC.StreamInput(ctx)
	grpc.SetUserHeader(in.RequestHeader(), "root")
	if sbx.Config.Envd.AccessToken != nil {
		in.RequestHeader().Set("X-Access-Token", *sbx.Config.Envd.AccessToken)
	}

	if err := in.Send(&process.StreamInputRequest{
		Event: &process.StreamInputRequest_Start{
			Start: &process.StreamInputRequest_StartEvent{
				Process: &process.ProcessSelector{
					Selector: &process.ProcessSelector_Pid{Pid: pid},
				},
			},
		},
	}); err != nil {
		return
	}

	buf := make([]byte, 4096)
	for {
		// In raw mode, Read blocks until a byte arrives. We can't easily
		// interrupt it on ctx.Done, but the parent process will exit soon
		// after the stream closes, which is acceptable for a CLI.
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			if sendErr := in.Send(&process.StreamInputRequest{
				Event: &process.StreamInputRequest_Data{
					Data: &process.StreamInputRequest_DataEvent{
						Input: &process.ProcessInput{
							Input: &process.ProcessInput_Pty{Pty: data},
						},
					},
				},
			}); sendErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func pumpResize(
	ctx context.Context,
	processC processconnect.ProcessClient,
	sbx *sandbox.Sandbox,
	pid uint32,
) {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	for {
		select {
		case <-ctx.Done():
			return
		case <-winch:
			cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
			if err != nil || cols == 0 || rows == 0 {
				continue
			}
			req := connect.NewRequest(&process.UpdateRequest{
				Process: &process.ProcessSelector{
					Selector: &process.ProcessSelector_Pid{Pid: pid},
				},
				Pty: &process.PTY{
					Size: &process.PTY_Size{Cols: uint32(cols), Rows: uint32(rows)},
				},
			})
			grpc.SetUserHeader(req.Header(), "root")
			if sbx.Config.Envd.AccessToken != nil {
				req.Header().Set("X-Access-Token", *sbx.Config.Envd.AccessToken)
			}
			updateCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, _ = processC.Update(updateCtx, req)
			cancel()
		}
	}
}

func endToError(end *process.ProcessEvent_EndEvent) error {
	if end == nil {
		return nil
	}

	return &shellExitedError{exitCode: end.GetExitCode()}
}
```

### 4.3 Integration-test envd client (headers for proxy access, `X-Access-Token`)

`tests/integration/internal/setup/envd_client.go` lines 1-77 (of 77)

```go
package setup

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/grpc"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/filesystem/filesystemconnect"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process/processconnect"
	"github.com/e2b-dev/infra/tests/integration/internal/envd"
)

type EnvdClient struct {
	HTTPClient       *envd.ClientWithResponses
	FilesystemClient filesystemconnect.FilesystemClient
	ProcessClient    processconnect.ProcessClient
}

func GetEnvdClient(tb testing.TB, _ context.Context) *EnvdClient {
	tb.Helper()

	hc := http.Client{
		Timeout: envdTimeout,
	}

	httpC, err := envd.NewClientWithResponses(EnvdProxy, envd.WithHTTPClient(&hc))
	require.NoError(tb, err)

	fileC := filesystemconnect.NewFilesystemClient(&hc, EnvdProxy)
	processC := processconnect.NewProcessClient(&hc, EnvdProxy)

	return &EnvdClient{
		HTTPClient:       httpC,
		FilesystemClient: fileC,
		ProcessClient:    processC,
	}
}

func WithSandbox(tb testing.TB, sandboxID string) func(context.Context, *http.Request) error {
	tb.Helper()

	return func(_ context.Context, req *http.Request) error {
		SetSandboxHeader(tb, req.Header, sandboxID)
		req.Host = req.Header.Get("Host")

		return nil
	}
}

func WithEnvdAccessToken(tb testing.TB, accessToken string) func(ctx context.Context, req *http.Request) error {
	tb.Helper()

	return func(_ context.Context, req *http.Request) error {
		SetAccessTokenHeader(tb, req.Header, accessToken)

		return nil
	}
}

func SetSandboxHeader(tb testing.TB, header http.Header, sandboxID string) {
	tb.Helper()
	err := grpc.SetSandboxHeader(header, EnvdProxy, sandboxID)
	require.NoError(tb, err)
}

func SetAccessTokenHeader(tb testing.TB, header http.Header, accessToken string) {
	tb.Helper()
	header.Set("X-Access-Token", accessToken)
}

func SetUserHeader(tb testing.TB, header http.Header, user string) {
	tb.Helper()
	grpc.SetUserHeader(header, user)
}
```

### 4.4 envd server side: auth header, excluded paths, Basic-auth user, keepalive and timeout headers

`X-Access-Token` is required on everything except `GET /health`, `GET /files`, `POST /files`,
`POST /init` once a token is set (the /files routes then need either the header or a signature).
If no Basic user is sent, envd uses its default user (set by /init; the template build default is `user`).

`packages/envd/internal/api/auth.go` lines 1-163 (of 163)

```go
package api

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/awnumar/memguard"

	"github.com/e2b-dev/infra/packages/shared/pkg/keys"
)

const (
	SigningReadOperation  = "read"
	SigningWriteOperation = "write"

	accessTokenHeader = "X-Access-Token"
)

// paths that are always allowed without general authentication
// POST/init is secured via MMDS hash validation instead
var authExcludedPaths = []string{
	"GET/health",
	"GET/files",
	"POST/files",
	"POST/init",
}

// handoverPreInitAllowedPaths is the MINIMAL set reachable on a live-upgraded
// envd before its post-upgrade /init has restored the access token: only /init
// (which restores auth and lifts this gate, self-authenticated via MMDS) and
// the health check. It deliberately omits /files — unlike authExcludedPaths —
// so a re-adopted (possibly hostile) guest process can't reach the
// root-privileged file API unauthenticated in that window. The orchestrator
// delivers the upgrade over /upgrade's body, not /files, so nothing legitimate
// needs /files before /init.
var handoverPreInitAllowedPaths = []string{
	"GET/health",
	"POST/init",
}

func (a *API) WithAuthorization(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// check if this path is allowed without authentication (e.g., health check, endpoints supporting signing)
		allowedPath := slices.Contains(authExcludedPaths, req.Method+req.URL.Path)

		switch {
		case a.accessToken.IsSet():
			authHeader := req.Header.Get(accessTokenHeader)

			if !a.accessToken.Equals(authHeader) && !allowedPath {
				a.logger.Error().Msg("Trying to access secured envd without correct access token")

				err := errors.New("unauthorized access, please provide a valid access token or method signing if supported")
				jsonError(w, http.StatusUnauthorized, err)

				return
			}

		case a.handover != nil && !a.initialized.Load() && !slices.Contains(handoverPreInitAllowedPaths, req.Method+req.URL.Path):
			// A live-upgraded envd serves before its post-upgrade /init has
			// restored the access token — and the fallback thaw may already be
			// running the re-adopted workload. The sandbox HAD a token (it is a
			// resume), so treat the unset token as "not yet restored" and fail
			// CLOSED here rather than falling through to the open path below,
			// which would let a re-adopted (and possibly hostile) guest process
			// reach control endpoints unauthenticated in this window. Only the
			// minimal handoverPreInitAllowedPaths (/init, /health) get through —
			// notably NOT /files, whose root file API is otherwise unauthenticated
			// (it is in authExcludedPaths). /init self-authenticates via MMDS and
			// lifts this gate.
			a.logger.Warn().Msg("blocking pre-init request on live-upgraded envd (auth not yet restored)")

			jsonError(w, http.StatusUnauthorized, errors.New("envd not initialized"))

			return
		}

		handler.ServeHTTP(w, req)
	})
}

func (a *API) generateSignature(path string, username string, operation string, signatureExpiration *int64) (string, error) {
	tokenBytes, err := a.accessToken.Bytes()
	if err != nil {
		return "", fmt.Errorf("access token is not set: %w", err)
	}
	defer memguard.WipeBytes(tokenBytes)

	var signature string
	hasher := keys.NewSHA256Hashing()

	if signatureExpiration == nil {
		signature = strings.Join([]string{path, operation, username, string(tokenBytes)}, ":")
	} else {
		signature = strings.Join([]string{path, operation, username, string(tokenBytes), strconv.FormatInt(*signatureExpiration, 10)}, ":")
	}

	return fmt.Sprintf("v1_%s", hasher.HashWithoutPrefix([]byte(signature))), nil
}

func (a *API) validateSigning(r *http.Request, signature *string, signatureExpiration *int, username *string, path string, operation string) (err error) {
	var expectedSignature string

	// no need to validate signing key if access token is not set
	if !a.accessToken.IsSet() {
		return nil
	}

	// check if access token is sent in the header
	tokenFromHeader := r.Header.Get(accessTokenHeader)
	if tokenFromHeader != "" {
		if !a.accessToken.Equals(tokenFromHeader) {
			return errors.New("access token present in header but does not match")
		}

		return nil
	}

	if signature == nil {
		return errors.New("missing signature query parameter")
	}

	// Empty string is used when no username is provided and the default user should be used
	signatureUsername := ""
	if username != nil {
		signatureUsername = *username
	}

	if signatureExpiration == nil {
		expectedSignature, err = a.generateSignature(path, signatureUsername, operation, nil)
	} else {
		exp := int64(*signatureExpiration)
		expectedSignature, err = a.generateSignature(path, signatureUsername, operation, &exp)
	}

	if err != nil {
		a.logger.Error().Err(err).Msg("error generating signing key")

		return errors.New("invalid signature")
	}

	// signature validation
	// Use constant-time comparison to prevent timing attacks.
	if subtle.ConstantTimeCompare([]byte(expectedSignature), []byte(*signature)) != 1 {
		return errors.New("invalid signature")
	}

	// signature expiration
	if signatureExpiration != nil {
		exp := int64(*signatureExpiration)
		if exp < time.Now().Unix() {
			return errors.New("signature is already expired")
		}
	}

	return nil
}
```

`packages/envd/internal/permissions/authenticate.go` lines 1-47 (of 47)

```go
package permissions

import (
	"context"
	"errors"
	"os/user"

	"connectrpc.com/authn"
	"connectrpc.com/connect"

	"github.com/e2b-dev/infra/packages/envd/internal/execcontext"
)

func AuthenticateUsername(_ context.Context, req authn.Request) (any, error) {
	username, _, ok := req.BasicAuth()
	if !ok {
		// When no username is provided, ignore the authentication method (not all endpoints require it)
		// Missing user is then handled in the GetAuthUser function
		return nil, nil
	}

	u, err := GetUser(username)
	if err != nil {
		return nil, authn.Errorf("invalid username: '%s'", username)
	}

	return u, nil
}

func GetAuthUser(ctx context.Context, defaultUser string) (*user.User, error) {
	u, ok := authn.GetInfo(ctx).(*user.User)
	if !ok {
		username, err := execcontext.ResolveDefaultUsername(nil, defaultUser)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("no user specified"))
		}

		u, err := GetUser(username)
		if err != nil {
			return nil, authn.Errorf("invalid default user: '%s'", username)
		}

		return u, nil
	}

	return u, nil
}
```

`packages/envd/internal/permissions/keepalive.go` lines 1-29 (of 29)

```go
package permissions

import (
	"strconv"
	"time"

	"connectrpc.com/connect"
)

const defaultKeepAliveInterval = 90 * time.Second

func GetKeepAliveTicker[T any](req *connect.Request[T]) (*time.Ticker, func()) {
	keepAliveIntervalHeader := req.Header().Get("Keepalive-Ping-Interval")

	var interval time.Duration

	keepAliveIntervalInt, err := strconv.Atoi(keepAliveIntervalHeader)
	if err != nil {
		interval = defaultKeepAliveInterval
	} else {
		interval = time.Duration(keepAliveIntervalInt) * time.Second
	}

	ticker := time.NewTicker(interval)

	return ticker, func() {
		ticker.Reset(interval)
	}
}
```

Process start (user resolution, `Connect-Timeout-Ms` => process kill deadline, keepalive events):

`packages/envd/internal/services/process/start.go` lines 23-45 (of 235)

```go
func (s *Service) handleStart(ctx context.Context, req *connect.Request[rpc.StartRequest], stream *connect.ServerStream[rpc.StartResponse]) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	handlerL := s.logger.With().Str(string(logs.OperationIDKey), ctx.Value(logs.OperationIDKey).(string)).Logger()

	u, err := permissions.GetAuthUser(ctx, s.defaults.User)
	if err != nil {
		return err
	}

	requestTimeout, err := determineTimeoutFromHeader(stream.Conn().RequestHeader())
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}

	// Create a new context with a timeout if provided.
	// We do not want the command to be killed if the request context is cancelled
	procCtx, cancelProc := context.Background(), func() {}
	if requestTimeout > 0 { // zero timeout means no timeout
		procCtx, cancelProc = context.WithTimeout(procCtx, requestTimeout)
	}

```

`packages/envd/internal/services/process/start.go` lines 105-125 (of 235)

```go
		keepaliveTicker, resetKeepalive := permissions.GetKeepAliveTicker(req)
		defer keepaliveTicker.Stop()

	dataLoop:
		for {
			select {
			case <-keepaliveTicker.C:
				streamErr := stream.Send(&rpc.StartResponse{
					Event: &rpc.ProcessEvent{
						Event: &rpc.ProcessEvent_Keepalive{
							Keepalive: &rpc.ProcessEvent_KeepAlive{},
						},
					},
				})
				if streamErr != nil {
					cancel(connect.NewError(connect.CodeUnknown, fmt.Errorf("error sending keepalive: %w", streamErr)))

					return
				}
			case <-ctx.Done():
				cancel(ctx.Err())
```

`packages/envd/internal/services/process/start.go` lines 222-235 (of 235)

```go
func determineTimeoutFromHeader(header http.Header) (time.Duration, error) {
	timeoutHeader := header.Get("Connect-Timeout-Ms")

	if timeoutHeader == "" {
		return 0, nil
	}

	timeout, err := strconv.Atoi(timeoutHeader)
	if err != nil {
		return 0, err
	}

	return time.Duration(timeout) * time.Millisecond, nil
}
```

Process execution details (wrapped in `/bin/sh -c`, runs as the uid/gid of the auth user, env =
PATH/HOME/USER/LOGNAME + envd defaults + request envs, PTY via creack/pty; stdin pipe unless
`stdin=false`):

`packages/envd/internal/services/process/handler/handler.go` lines 184-330 (of 623)

```go

func New(
	ctx context.Context,
	user *user.User,
	req *rpc.StartRequest,
	logger *zerolog.Logger,
	defaults *execcontext.Defaults,
	cgroupManager cgroups.Manager,
	cancel context.CancelFunc,
) (*Handler, error) {
	// User command string for logging (without the internal wrapper details).
	userCmd := strings.Join(append([]string{req.GetProcess().GetCmd()}, req.GetProcess().GetArgs()...), " ")

	// Wrap in a shell that resets oom_score_adj, ioprio (ionice best-effort/4),
	// and nice. The oom_score_adj write is pure /proc and always applied; the
	// priority helpers are used only where the image provides them.
	niceDelta := defaultNice - currentNice()
	oomWrapperScript := fmt.Sprintf(`echo %d > /proc/$$/oom_score_adj && exec %s"${@}"`, defaultOomScore, ioniceNicePrefix(defaultIoClass, defaultIoPrio, niceDelta, exec.LookPath))
	wrapperArgs := append([]string{"-c", oomWrapperScript, "--", req.GetProcess().GetCmd()}, req.GetProcess().GetArgs()...)
	cmd := exec.CommandContext(ctx, "/bin/sh", wrapperArgs...)

	uid, gid, err := permissions.GetUserIdUints(user)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	groups := []uint32{gid}
	if gids, err := user.GroupIds(); err != nil {
		logger.Warn().Err(err).Str("user", user.Username).Msg("failed to get supplementary groups")
	} else {
		for _, g := range gids {
			if parsed, err := strconv.ParseUint(g, 10, 32); err == nil {
				groups = append(groups, uint32(parsed))
			}
		}
	}

	cgroupFD, ok := cgroupManager.GetFileDescriptor(getProcType(req))

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid:    uid,
			Gid:    gid,
			Groups: groups,
		},
	}
	applyCgroupFD(cmd.SysProcAttr, cgroupFD, ok)

	resolvedPath, err := permissions.ExpandAndResolve(req.GetProcess().GetCwd(), user, defaults.Workdir)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	// Check if the cwd resolved path exists
	if _, err := os.Stat(resolvedPath); errors.Is(err, os.ErrNotExist) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("cwd '%s' does not exist", resolvedPath))
	}

	cmd.Dir = resolvedPath

	var formattedVars []string

	// Take only 'PATH' variable from the current environment
	// The 'PATH' should ideally be set in the environment
	formattedVars = append(formattedVars, "PATH="+os.Getenv("PATH"))
	formattedVars = append(formattedVars, "HOME="+user.HomeDir)
	formattedVars = append(formattedVars, "USER="+user.Username)
	formattedVars = append(formattedVars, "LOGNAME="+user.Username)

	// Add the environment variables from the global environment
	if defaults.EnvVars != nil {
		for key, value := range defaults.EnvVars.All() {
			formattedVars = append(formattedVars, key+"="+value)
		}
	}

	// Only the last values of the env vars are used - this allows for overwriting defaults
	for key, value := range req.GetProcess().GetEnvs() {
		formattedVars = append(formattedVars, key+"="+value)
	}

	cmd.Env = formattedVars

	outMultiplex := NewMultiplexedChannel[rpc.ProcessEvent_Data](outputBufferSize)

	var outWg sync.WaitGroup

	// Create a context for waiting for and cancelling output pipes.
	// Cancellation of the process via timeout will propagate and cancel this context too.
	outCtx, outCancel := context.WithCancel(ctx)

	h := &Handler{
		Config:    req.GetProcess(),
		cmd:       cmd,
		Tag:       req.Tag,
		DataEvent: outMultiplex,
		cancel:    cancel,
		outCtx:    outCtx,
		outCancel: outCancel,
		EndEvent:  NewMultiplexedChannel[rpc.ProcessEvent_End](0),
		logger:    logger,
	}
	h.cgType = getProcType(req)

	// Capture the process timeout deadline (if any) so it can be carried across
	// a live-upgrade and re-armed on the new envd.
	if d, ok := ctx.Deadline(); ok {
		h.setDeadline(d)
	}

	if req.GetPty() != nil {
		// The pty should ideally start only in the Start method, but the package does not support that and we would have to code it manually.
		// The output of the pty should correctly be passed though.
		tty, err := pty.StartWithSize(cmd, &pty.Winsize{
			Cols: uint16(req.GetPty().GetSize().GetCols()),
			Rows: uint16(req.GetPty().GetSize().GetRows()),
		})
		if err != nil {
			startErr := fmt.Errorf("error starting pty with command '%s' in dir '%s' with '%d' cols and '%d' rows: %w", userCmd, cmd.Dir, req.GetPty().GetSize().GetCols(), req.GetPty().GetSize().GetRows(), err)

			return nil, connect.NewError(StartErrorCode(startErr), startErr)
		}

		outWg.Go(func() {
			readBuf := make([]byte, ptyChunkSize)

			for {
				n, readErr := tty.Read(readBuf)

				if n > 0 {
					h.ptyBytes.Add(int64(n))

					if outMultiplex.HasSubscribers() {
						data := slices.Clone(readBuf[:n])

						outMultiplex.Source <- rpc.ProcessEvent_Data{
							Data: &rpc.ProcessEvent_DataEvent{
								Output: &rpc.ProcessEvent_DataEvent_Pty{
									Pty: data,
								},
							},
						}
					}
				}

				if errors.Is(readErr, io.EOF) || errors.Is(readErr, syscall.EIO) {
					break
```

`packages/envd/internal/services/process/handler/handler.go` lines 425-435 (of 623)

```go
		})

		// For backwards compatibility we still set the stdin if not explicitly disabled
		// If stdin is disabled, the process will use /dev/null as stdin
		if req.Stdin == nil || req.GetStdin() == true {
			stdin, err := cmd.StdinPipe()
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("error creating stdin pipe for command '%s': %w", userCmd, err))
			}

			h.stdin = stdin
```

`packages/envd/internal/services/process/update.go` lines 1-30 (of 30)

```go
package process

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"github.com/creack/pty"

	rpc "github.com/e2b-dev/infra/packages/envd/internal/services/spec/process"
)

func (s *Service) Update(_ context.Context, req *connect.Request[rpc.UpdateRequest]) (*connect.Response[rpc.UpdateResponse], error) {
	proc, err := s.getProcess(req.Msg.GetProcess())
	if err != nil {
		return nil, err
	}

	if req.Msg.GetPty() != nil {
		err := proc.ResizeTty(&pty.Winsize{
			Rows: uint16(req.Msg.GetPty().GetSize().GetRows()),
			Cols: uint16(req.Msg.GetPty().GetSize().GetCols()),
		})
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("error resizing tty: %w", err))
		}
	}

	return connect.NewResponse(&rpc.UpdateResponse{}), nil
}
```

`packages/envd/internal/services/process/input.go` lines 1-100 (of 100)

```go
package process

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	"github.com/e2b-dev/infra/packages/envd/internal/logs"
	"github.com/e2b-dev/infra/packages/envd/internal/services/process/handler"
	rpc "github.com/e2b-dev/infra/packages/envd/internal/services/spec/process"
)

func handleInput(process *handler.Handler, in *rpc.ProcessInput) error {
	switch in.GetInput().(type) {
	case *rpc.ProcessInput_Pty:
		err := process.WriteTty(in.GetPty())
		if err != nil {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("error writing to tty: %w", err))
		}

	case *rpc.ProcessInput_Stdin:
		err := process.WriteStdin(in.GetStdin())
		if err != nil {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("error writing to stdin: %w", err))
		}

	default:
		return connect.NewError(connect.CodeUnimplemented, fmt.Errorf("invalid input type %T", in.GetInput()))
	}

	return nil
}

func (s *Service) SendInput(_ context.Context, req *connect.Request[rpc.SendInputRequest]) (*connect.Response[rpc.SendInputResponse], error) {
	proc, err := s.getProcess(req.Msg.GetProcess())
	if err != nil {
		return nil, err
	}

	err = handleInput(proc, req.Msg.GetInput())
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&rpc.SendInputResponse{}), nil
}

func (s *Service) StreamInput(ctx context.Context, stream *connect.ClientStream[rpc.StreamInputRequest]) (*connect.Response[rpc.StreamInputResponse], error) {
	return logs.LogClientStreamWithoutEvents(ctx, s.logger, stream, s.streamInputHandler)
}

func (s *Service) streamInputHandler(_ context.Context, stream *connect.ClientStream[rpc.StreamInputRequest]) (*connect.Response[rpc.StreamInputResponse], error) {
	var proc *handler.Handler

	for stream.Receive() {
		req := stream.Msg()

		switch req.GetEvent().(type) {
		case *rpc.StreamInputRequest_Start:
			p, err := s.getProcess(req.GetStart().GetProcess())
			if err != nil {
				return nil, err
			}

			proc = p
		case *rpc.StreamInputRequest_Data:
			err := handleInput(proc, req.GetData().GetInput())
			if err != nil {
				return nil, err
			}
		case *rpc.StreamInputRequest_Keepalive:
		default:
			return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("invalid event type %T", req.GetEvent()))
		}
	}

	err := stream.Err()
	if err != nil {
		return nil, connect.NewError(connect.CodeUnknown, fmt.Errorf("error streaming input: %w", err))
	}

	return connect.NewResponse(&rpc.StreamInputResponse{}), nil
}

func (s *Service) CloseStdin(
	_ context.Context,
	req *connect.Request[rpc.CloseStdinRequest],
) (*connect.Response[rpc.CloseStdinResponse], error) {
	handler, err := s.getProcess(req.Msg.GetProcess())
	if err != nil {
		return nil, err
	}

	if err := handler.CloseStdin(); err != nil {
		return nil, connect.NewError(connect.CodeUnknown, fmt.Errorf("error closing stdin: %w", err))
	}

	return connect.NewResponse(&rpc.CloseStdinResponse{}), nil
}
```

### 4.5 (context) What the orchestrator itself sends to envd `/init` after every start/resume

A client never calls /init; the orchestrator does it at the slot IP with the `envd_access_token`,
`env_vars` and default user from the create request:

`packages/orchestrator/pkg/sandbox/envd.go` lines 79-135 (of 888)

```go
func (s *Sandbox) doRequestWithInfiniteRetries(
	ctx context.Context,
	method,
	address string,
) (*http.Response, int64, error) {
	requestCount := int64(0)

	jsonBody := &envd.PostInitJSONBody{
		LifecycleID:    s.LifecycleID,
		EnvVars:        s.Config.Envd.Vars,
		HyperloopIP:    s.config.NetworkConfig.OrchestratorInSandboxIPAddress,
		AccessToken:    utils.DerefOrDefault(s.Config.Envd.AccessToken, ""),
		DefaultUser:    utils.DerefOrDefault(s.Config.Envd.DefaultUser, ""),
		DefaultWorkdir: utils.DerefOrDefault(s.Config.Envd.DefaultWorkdir, ""),
		VolumeMounts:   s.convertMounts(s.Config.VolumeMounts),
		CaBundle:       s.CABundle,
	}

	for {
		jsonBody.Timestamp = time.Now()

		body, err := json.Marshal(jsonBody)
		if err != nil {
			return nil, requestCount, err
		}

		requestCount++
		reqCtx, cancel := context.WithTimeout(ctx, s.internalConfig.EnvdInitRequestTimeout)
		request, err := http.NewRequestWithContext(reqCtx, method, address, bytes.NewReader(body))
		if err != nil {
			cancel()

			return nil, requestCount, err
		}

		// make sure request to already authorized envd will not fail
		// this can happen in sandbox resume and in some edge cases when previous request was success, but we continued
		if s.Config.Envd.AccessToken != nil {
			request.Header.Set("X-Access-Token", *s.Config.Envd.AccessToken)
		}

		response, err := sandboxHttpClient.Do(request)
		cancel()

		if err == nil {
			return response, requestCount, nil
		}

		select {
		case <-ctx.Done():
			return nil, requestCount, fmt.Errorf("%w with cause: %w", ctx.Err(), context.Cause(ctx))
		case <-time.After(loopDelay):
		}
	}
}

// callEnvdFreeze issues the pre-pause freeze through envd's native POST /freeze --
```

---

## 5. Orchestrator sandbox proxy routing

### 5.1 Target parsing: `packages/shared/pkg/proxy/host.go` (complete)

Headers are consulted only when the request Host is `localhost`, an IP literal, or `sandbox.<domain>`;
otherwise the Host is parsed as `<port>-<sandboxID>[-...].<domain>`. The sandbox ID is validated with
`^[a-z0-9]+$`.

`packages/shared/pkg/proxy/host.go` lines 1-136 (of 136)

```go
package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/e2b-dev/infra/packages/shared/pkg/id"
)

const sandboxSharedHostSubdomain = "sandbox."

func GetTargetFromRequest() func(r *http.Request) (sandboxId string, port uint64, err error) {
	return func(r *http.Request) (sandboxId string, port uint64, err error) {
		if shouldParseHeaders(r.Host) && hasRoutingHeaders(r.Header) {
			var ok bool
			sandboxId, port, ok, err = parseHeaders(r.Header)
			if err != nil {
				return "", 0, err
			} else if ok {
				if err := id.ValidateSandboxID(sandboxId); err != nil {
					return "", 0, ErrInvalidSandboxID
				}

				return sandboxId, port, nil
			}
		}

		sandboxId, port, err = parseHost(r.Host)
		if err != nil {
			return "", 0, err
		}

		if err := id.ValidateSandboxID(sandboxId); err != nil {
			return "", 0, ErrInvalidSandboxID
		}

		return sandboxId, port, nil
	}
}

func shouldParseHeaders(host string) bool {
	_, sharedHost := SandboxSharedHostDomain(host)

	return isLocalRequestHost(host) || sharedHost
}

func requestHostname(host string) string {
	return (&url.URL{Host: host}).Hostname()
}

func isLocalRequestHost(host string) bool {
	host = requestHostname(host)
	ip := net.ParseIP(host)

	// An IP address cannot encode sandbox routing info (like {port}-{sandboxId}.{domain}),
	// so header-based routing is the only mechanism that can work for IP hosts.
	return host == "localhost" || ip != nil
}

func SandboxSharedHostDomain(host string) (string, bool) {
	domain, ok := strings.CutPrefix(requestHostname(host), sandboxSharedHostSubdomain)

	return domain, ok && domain != ""
}

func hasRoutingHeaders(h http.Header) bool {
	return h.Get(headerSandboxID) != "" || h.Get(headerSandboxPort) != ""
}

func parseHost(host string) (sandboxID string, port uint64, err error) {
	dot := strings.Index(host, ".")

	// There must be always domain part used
	if dot == -1 {
		return "", 0, ErrInvalidHost
	}

	// Keep only the left-most subdomain part, i.e. everything before the
	host = host[:dot]

	hostParts := strings.Split(host, "-")
	if len(hostParts) < 2 {
		return "", 0, ErrInvalidHost
	}

	sandboxPortString := hostParts[0]
	sandboxID = hostParts[1]

	sandboxPort, err := strconv.ParseUint(sandboxPortString, 10, 64)
	if err != nil {
		return "", 0, InvalidSandboxPortError{sandboxPortString, err}
	}

	return sandboxID, sandboxPort, nil
}

type MissingHeaderError struct {
	Header string
}

func (e MissingHeaderError) Error() string {
	return fmt.Sprintf("Missing header: %s", e.Header)
}

const (
	headerSandboxID   = "E2b-Sandbox-Id"
	headerSandboxPort = "E2b-Sandbox-Port"
)

func parseHeaders(h http.Header) (sandboxID string, port uint64, ok bool, err error) {
	sandboxID = h.Get(headerSandboxID)
	portString := h.Get(headerSandboxPort)

	if sandboxID == "" && portString == "" {
		return "", 0, false, nil
	}

	if sandboxID == "" {
		return "", 0, false, MissingHeaderError{Header: headerSandboxID}
	}

	if portString == "" {
		return "", 0, false, MissingHeaderError{Header: headerSandboxPort}
	}

	port, err = strconv.ParseUint(portString, 10, 64)
	if err != nil {
		return "", 0, false, InvalidSandboxPortError{portString, err}
	}

	return sandboxID, port, true, nil
}
```

Listener address (`:<port>`, all interfaces):

`packages/shared/pkg/proxy/proxy.go` lines 55-75 (of 147)

```go
	disableKeepAlives bool,
) *Proxy {
	p := pool.New(
		maxClientConns,
		int(maxConnectionAttempts),
		idleTimeout,
		disableKeepAlives,
	)

	proxy := &Proxy{
		Server: http.Server{
			Addr:         fmt.Sprintf(":%d", port),
			ReadTimeout:  0,
			WriteTimeout: 0,
			// Downstream idle timeout (client facing) > upstream idle timeout (server facing)
			// otherwise there's a chance for a race condition when the server closes and the client tries to use the connection
			IdleTimeout:       idleTimeout + idleTimeoutBufferUpstreamDownstream,
			ReadHeaderTimeout: 0,
			Handler:           handler(p, getDestination, connLimitConfig),
		},
		pool:              p,
```

### 5.2 Orchestrator proxy handler: `packages/orchestrator/pkg/proxy/proxy.go` (complete)

Traffic token header: `e2b-traffic-access-token` (only checked for non-envd ports, only when the sandbox
has `network.ingress.traffic_access_token`). Envd internal routes are refused before sandbox lookup.
Upstream is `http://<slotHostIP>:<port>` (https for ports listed in `ingress.https_ports`).

`packages/orchestrator/pkg/proxy/proxy.go` lines 1-271 (of 271)

```go
//go:build linux

package proxy

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/envd"
	"github.com/e2b-dev/infra/packages/shared/pkg/connlimit"
	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	reverseproxy "github.com/e2b-dev/infra/packages/shared/pkg/proxy"
	"github.com/e2b-dev/infra/packages/shared/pkg/proxy/pool"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

const (
	// This timeout should be > 600 (GCP LB upstream idle timeout) to prevent race condition
	// Also it's a good practice to set it to higher values as you progress in the stack
	// https://cloud.google.com/load-balancing/docs/https#timeouts_and_retries%23:~:text=The%20load%20balancer%27s%20backend%20keepalive,is%20greater%20than%20600%20seconds
	idleTimeout     = 620 * time.Second
	shutdownTimeout = 30 * time.Second

	trafficAccessTokenHeader = "e2b-traffic-access-token"
)

var _ sandbox.MapSubscriber = (*SandboxProxy)(nil)

// envd speaks plaintext, so its port stays HTTP even when the config lists it.
// The API refuses it at create time; this covers direct gRPC callers.
func schemeForPort(ingress *orchestrator.SandboxNetworkIngressConfig, port uint64) string {
	if port == uint64(consts.DefaultEnvdServerPort) {
		return "http"
	}

	for _, httpsPort := range ingress.GetHttpsPorts() {
		if uint64(httpsPort) == port {
			return "https"
		}
	}

	return "http"
}

type SandboxProxy struct {
	proxy   *reverseproxy.Proxy
	limiter *connlimit.ConnectionLimiter
}

// newDestinationResolver returns the proxy's routing decision: which sandbox and
// port a request is for, or why it is refused.
func newDestinationResolver(sandboxes *sandbox.Map) func(r *http.Request) (*pool.Destination, error) {
	getTargetFromRequest := reverseproxy.GetTargetFromRequest()

	return func(r *http.Request) (*pool.Destination, error) {
		sandboxId, port, err := getTargetFromRequest(r)
		if err != nil {
			return nil, err
		}

		isNonEnvdTraffic := int64(port) != consts.DefaultEnvdServerPort

		// envd serves the orchestrator's control plane (/init, the pause
		// freeze/thaw hooks, the live self-upgrade) on the same port as the
		// sandbox's public surface. The orchestrator calls those routes directly
		// at the slot IP and never through this proxy, so a request for one that
		// arrived here came from outside and has no business reaching envd —
		// envd's own access-token check would let the sandbox's owner wedge or
		// re-image their guest, and /init is exempt from that check altogether.
		//
		// Refused before the sandbox is looked up: the answer is the same whether
		// or not the addressed sandbox is on this node, so the reply carries
		// nothing about which sandboxes exist here.
		if !isNonEnvdTraffic && envd.IsInternalPath(r.URL.Path) {
			return nil, reverseproxy.NewErrInternalRoute(sandboxId, r.URL.Path)
		}

		sbx, found := sandboxes.Get(sandboxId)
		if !found {
			return nil, reverseproxy.NewErrSandboxNotFound(sandboxId)
		}

		ingress := sbx.Config.GetNetworkIngress()
		accessToken := ingress.GetTrafficAccessToken()

		// Handle traffic access token validation.
		// We are skipping envd port as it has its own access validation mechanism.
		// Preflights fail this check too — the Fetch standard forbids a preflight
		// from carrying custom headers, so it can never present the token. The
		// proxy answers them from the resulting error path, so no unauthenticated
		// request ever reaches the guest port.
		if accessToken != "" && isNonEnvdTraffic {
			accessTokenRaw := r.Header.Get(trafficAccessTokenHeader)
			if accessTokenRaw == "" {
				return nil, reverseproxy.NewErrMissingTrafficAccessToken(sandboxId, trafficAccessTokenHeader)
			} else if subtle.ConstantTimeCompare([]byte(accessTokenRaw), []byte(accessToken)) != 1 {
				return nil, reverseproxy.NewErrInvalidTrafficAccessToken(sandboxId, trafficAccessTokenHeader)
			}
		}

		// Handle request host masking only for non-envd traffic.
		var maskRequestHost *string = nil
		if h := ingress.GetMaskRequestHost(); isNonEnvdTraffic && h != "" {
			h = strings.ReplaceAll(h, pool.MaskRequestHostPortPlaceholder, strconv.FormatUint(port, 10))
			maskRequestHost = &h
		}

		url := &url.URL{
			Scheme: schemeForPort(ingress, port),
			Host:   net.JoinHostPort(sbx.Slot.HostIPString(), strconv.FormatUint(port, 10)),
		}

		logger := logger.L().With(
			append(
				logger.ProxyRequestFields(r, sbx.Runtime.SandboxID, port),
				logger.WithTeamID(sbx.Runtime.TeamID),
				logger.WithSandboxIP(sbx.Slot.HostIPString()),
			)...,
		)

		return &pool.Destination{
			Url:                                url,
			SandboxId:                          sbx.Runtime.SandboxID,
			SandboxPort:                        port,
			DefaultToPortError:                 true,
			IncludeSandboxIdInProxyErrorLogger: true,
			// We need to include id unique to sandbox to prevent reuse of connection to the same IP:port pair by different sandboxes reusing the network slot.
			// We are not using sandbox id to prevent removing connections based on sandbox id (pause/resume race condition).
			ConnectionKey:   sbx.LifecycleID,
			RequestLogger:   logger,
			MaskRequestHost: maskRequestHost,
			// Guest certificates are self-signed, so verification cannot
			// succeed. Safe to decide per destination because ingress is fixed
			// for the lifecycle the pool keys its clients by.
			InsecureSkipTLSVerify: len(ingress.GetHttpsPorts()) > 0,
		}, nil
	}
}

func NewSandboxProxy(meterProvider metric.MeterProvider, port uint16, sandboxes *sandbox.Map, featureFlags *featureflags.Client) (*SandboxProxy, error) {
	limiter := connlimit.NewConnectionLimiter()
	metrics := NewMetrics(meterProvider)

	meter := meterProvider.Meter("github.com/e2b-dev/infra/packages/orchestrator/pkg/proxy")

	connLimitConfig := &reverseproxy.ConnectionLimitConfig{
		Limiter: limiter,
		GetMaxLimit: func(ctx context.Context) int {
			return featureFlags.IntFlag(ctx, featureflags.SandboxMaxIncomingConnections)
		},
		OnConnectionAcquired: metrics.RecordConnectionsPerSandbox,
		OnConnectionReleased: metrics.RecordConnectionDuration,
		OnConnectionBlocked:  metrics.RecordConnectionBlocked,
	}

	proxy := reverseproxy.New(
		port,
		// Retry 5 times to handle port forwarding delays in sandbox envd.
		reverseproxy.SandboxProxyRetries,
		idleTimeout,
		newDestinationResolver(sandboxes),
		connLimitConfig,
		// We are not using keepalives for orchestrator proxy,
		// because the servers inside of the sandbox can be unstable (restarts),
		// and we are also on the same host, so the overhead is minimal.
		true,
	)

	_, err := telemetry.GetObservableUpDownCounter(meter, telemetry.OrchestratorProxyServerConnectionsMeterCounterName, func(_ context.Context, observer metric.Int64Observer) error {
		observer.Observe(proxy.CurrentServerConnections())

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("error registering orchestrator proxy connections metric (%s): %w", telemetry.OrchestratorProxyServerConnectionsMeterCounterName, err)
	}

	_, err = telemetry.GetObservableUpDownCounter(meter, telemetry.OrchestratorProxyPoolConnectionsMeterCounterName, func(_ context.Context, observer metric.Int64Observer) error {
		observer.Observe(proxy.CurrentPoolConnections())

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("error registering orchestrator proxy connections metric (%s): %w", telemetry.OrchestratorProxyPoolConnectionsMeterCounterName, err)
	}

	_, err = telemetry.GetObservableUpDownCounter(meter, telemetry.OrchestratorProxyPoolSizeMeterCounterName, func(_ context.Context, observer metric.Int64Observer) error {
		observer.Observe(int64(proxy.CurrentPoolSize()))

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("error registering orchestrator proxy pool size metric (%s): %w", telemetry.OrchestratorProxyPoolSizeMeterCounterName, err)
	}

	sandboxProxy := &SandboxProxy{
		proxy:   proxy,
		limiter: limiter,
	}

	// Subscribe to sandbox events for cleanup
	sandboxes.Subscribe(sandboxProxy)

	return sandboxProxy, nil
}

func (p *SandboxProxy) Start(ctx context.Context) error {
	return p.proxy.ListenAndServe(ctx)
}

func (p *SandboxProxy) Close(ctx context.Context) error {
	var err error
	forced := ctx.Err() != nil
	if !forced {
		shutdownCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()

		err = p.proxy.Shutdown(shutdownCtx)
		if err != nil {
			forced = true
			logger.L().Warn(ctx, "sandbox proxy graceful shutdown interrupted", zap.Error(err))
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				err = nil
			}
		}
	}
	if forced {
		err = errors.Join(err, p.proxy.Close())
	}
	logger.L().Info(ctx, "sandbox proxy shutdown complete", zap.Bool("forced", forced), zap.Error(err))
	if err != nil {
		return fmt.Errorf("failed to shutdown proxy server: %w", err)
	}

	return nil
}

func (p *SandboxProxy) RemoveFromPool(connectionKey string) error {
	return p.proxy.RemoveFromPool(connectionKey)
}

func (p *SandboxProxy) GetAddr() string {
	return p.proxy.Addr
}

// OnInsert is called when a sandbox is inserted into the map.
func (p *SandboxProxy) OnInsert(_ context.Context, _ *sandbox.Sandbox) {}

// OnStopping is called when a sandbox leaves the live registry.
func (p *SandboxProxy) OnStopping(_ context.Context, _ *sandbox.Sandbox) {}

// OnNetworkRelease is called when a sandbox's network slot is released.
// Keyed by LifecycleID so the removal is scoped to this sandbox lifecycle.
func (p *SandboxProxy) OnNetworkRelease(_ context.Context, sbx *sandbox.Sandbox) {
	p.limiter.Remove(sbx.LifecycleID)
}
```

### 5.3 Blocked envd control routes

`packages/orchestrator/pkg/sandbox/envd/internal_routes.go` lines 1-65 (of 65)

```go
package envd

import (
	"path"
	"strings"
)

// unspecifiedInternalPaths are envd control routes the generator cannot see
// because the OpenAPI spec does not describe them. envd registers /upgrade
// directly on its mux rather than through the generated spec handlers, so
// marking it `x-internal` is not an option.
//
// Prefer describing a new control route in the spec over adding it here: a route
// in the spec is a route the generator keeps in sync on its own.
var unspecifiedInternalPaths = []string{
	"/upgrade",
}

var internalPaths = newInternalPathSet(specInternalPaths, unspecifiedInternalPaths)

func newInternalPathSet(sets ...[]string) map[string]struct{} {
	paths := make(map[string]struct{})

	for _, set := range sets {
		for _, p := range set {
			paths[p] = struct{}{}
		}
	}

	return paths
}

// IsInternalPath reports whether requestPath addresses an envd route reserved
// for the orchestrator's control plane. The orchestrator reaches envd over the
// host network at the sandbox slot IP, so a request for one of these paths that
// arrives through the public sandbox URL has no legitimate sender.
//
// The answer is deliberately method-agnostic: envd answers a known path with 405
// rather than 404 for the wrong method, which confirms the route exists, and a
// method added to a control route later would otherwise slip through.
//
// requestPath must be the decoded path — http.Request.URL.Path — and never
// RawPath, EscapedPath() or RequestURI. This does not percent-decode, so passing
// a still-escaped path reopens the very bypass it exists to close.
//
// Given the decoded path, cleaning before the lookup makes the answer a superset
// of what envd itself would route. envd's router matches the escaped path
// whenever it differs from the decoded one, and an escaped path differs from the
// decoded one exactly when it is not that path's canonical encoding — so the
// router can only ever reach a control route through a request whose decoded path
// is that route, spelled exactly. Cleaning only widens that: /init/, //init and
// /files/../init are refused here though envd would have 404'd them itself.
func IsInternalPath(requestPath string) bool {
	if requestPath == "" {
		return false
	}

	if !strings.HasPrefix(requestPath, "/") {
		requestPath = "/" + requestPath
	}

	_, internal := internalPaths[path.Clean(requestPath)]

	return internal
}
```

`packages/orchestrator/pkg/sandbox/envd/internal_routes.gen.go` lines 1-15 (of 15)

```go
// Code generated by gen_internal_routes.go from ../../../../envd/spec/envd.yaml; DO NOT EDIT.

package envd

// specInternalPaths are the envd request paths whose operations the envd
// OpenAPI spec marks `x-internal: true`: the orchestrator's control plane,
// which no request arriving through the public sandbox URL may reach.
var specInternalPaths = []string{
	"/collapse",
	"/freeze",
	"/fsfreeze",
	"/fsthaw",
	"/init",
	"/unfreeze",
}
```

---

## 6. Template build

### 6.1 `TemplateConfig` semantics (orchestrator-internal `config.TemplateConfig`, complete)

Mapping from the proto: `kernelVersion`, `firecrackerVersion`, `hugePages` in the proto are
**deprecated and ignored**; the template-manager resolves kernel/FC from its own flags
(`build-kernel-version`, default `DEFAULT_KERNEL_VERSION` env or `vmlinux-6.1.158`;
`build-firecracker-version`, default `DEFAULT_FIRECRACKER_VERSION` env or `v1.14-0.2.0`) and derives
hugePages from the FC version. `cacheScope` defaults to templateID; the API sends teamID. `version`
defaults to `v2.0.0` when absent. `freeDiskSizeMB` defaults to `diskSizeMB` when absent. Exactly one of
`fromImage`/`fromTemplate` is required (`InvalidArgument` otherwise). `TemplateCreate` returns
immediately; the build runs in a goroutine.

`packages/orchestrator/pkg/template/build/config/config.go` lines 1-113 (of 113)

```go
package config

import (
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/fc/cputemplate"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/core/oci/auth"
	"github.com/e2b-dev/infra/packages/shared/pkg/consts"
	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

const (
	InstanceBuildPrefix = "b"

	// TemplateDefaultUser is the default user to use in the template to run all commands.
	TemplateDefaultUser = consts.TemplateDefaultUser
)

type TemplateConfig struct {
	// Builder version
	Version string

	// TeamID is the ID of the team to build the template for.
	TeamID string

	// TemplateID is the ID of the template to build.
	TemplateID string

	// CacheScope is the scope of layers and files caches.
	CacheScope string

	// Command to run when building the template.
	StartCmd string

	// The number of vCPUs to allocate to the VM.
	VCpuCount int64

	// The amount of RAM memory to allocate to the VM, in MiB.
	MemoryMB int64

	// The amount of free rootfs working space provided before build steps, in MiB.
	DiskSizeMB int64

	// The amount of free rootfs target after build steps and before finalize, in MiB.
	// ext4 metadata and finalize writes may reduce the available space.
	FreeDiskSizeMB int64

	// HugePages sets whether the VM use huge pages.
	HugePages bool

	// FreePageReporting enables Firecracker's balloon free-page-reporting.
	FreePageReporting bool

	// FreePageHinting enables Firecracker's balloon free-page-hinting.
	FreePageHinting bool

	// Command to run to check if the template is ready.
	ReadyCmd string

	// FromImage is the base image to use for building the template.
	FromImage string

	// FromTemplate is the base template to use for building the template.
	FromTemplate *templatemanager.FromTemplateConfig

	// RegistryAuthProvider provides authentication for pulling the FromImage.
	RegistryAuthProvider auth.RegistryAuthProvider

	// Force rebuild of the template even if it is already cached.
	Force *bool

	// Steps to build the template.
	Steps []*templatemanager.TemplateStep

	// Firecracker version to use
	FirecrackerVersion string

	// Kernel version to use
	KernelVersion string

	// CmdlineArgs carries the extra guest kernel command line parameters this build boots
	// with, already parsed. Nil is the default command line. Read from the per-team feature
	// flag once, at the start of the build, so every boot in the build agrees and the
	// stored snapshot is self-describing.
	CmdlineArgs map[string]string

	// CPUTemplate is the CPU template every boot in this build applies, resolved once from
	// the per-team flag at build start. Nil is none.
	CPUTemplate *cputemplate.Template
}

// ObjectMetadata is the provenance stamped on a build's uploaded objects.
// buildOrigin distinguishes the final template build from intermediate
// build-cache layers.
func (e TemplateConfig) ObjectMetadata(buildOrigin storage.ObjectOrigin) storage.ObjectMetadata {
	return storage.ObjectMetadata{
		storage.ObjectMetadataTeamID:      e.TeamID,
		storage.ObjectMetadataTemplateID:  e.TemplateID,
		storage.ObjectMetadataBuildOrigin: string(buildOrigin),
	}
}

func MemfilePageSize(hugePages bool) int64 {
	if hugePages {
		return header.HugepageSize
	}

	return header.PageSize
}

func (e TemplateConfig) RootfsBlockSize() int64 {
	return header.RootfsBlockSize
}
```

`packages/orchestrator/pkg/template/server/create_template.go` lines 1-320 (of 320)

```go
//go:build linux

package server

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/fc"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/fc/cputemplate"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/builderrors"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/buildlogger"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/config"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/core/oci/auth"
	"github.com/e2b-dev/infra/packages/shared/pkg/fcversion"
	"github.com/e2b-dev/infra/packages/shared/pkg/featureflags"
	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/templates"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

var (
	meter = otel.Meter("github.com/e2b-dev/infra/packages/orchestrator/pkg/template/server")

	// buildCmdlineArgs is the engagement signal for the per-team cmdline flag: it counts
	// builds by the parameters they actually applied, so a flag that is set in the
	// feature-flag service but inert in the build shows up as the empty label rather
	// than as silence.
	buildCmdlineArgs = utils.Must(telemetry.GetCounter(meter, telemetry.TemplateBuildCmdlineArgs))

	// buildCPUTemplate counts builds by the digest of the CPU template applied and by result,
	// so a template the build rejected is not read as a team that has none.
	buildCPUTemplate = utils.Must(telemetry.GetCounter(meter, telemetry.TemplateBuildCPUTemplate))
)

func (s *ServerStore) TemplateCreate(ctx context.Context, templateRequest *templatemanager.TemplateCreateRequest) (*emptypb.Empty, error) {
	done := s.info.TrackWork()
	defer done()

	ctx, childSpan := tracer.Start(ctx, "template-create")
	defer childSpan.End()

	cfg := templateRequest.GetTemplate()

	if cfg.GetFromImage() == "" && cfg.GetFromTemplate() == nil {
		return nil, status.Error(codes.InvalidArgument, "template build requires either fromImage or fromTemplate")
	}

	metadata := storage.Paths{
		BuildID: cfg.GetBuildID(),
	}

	// default to scope by template ID
	cacheScope := cfg.GetTemplateID()
	if templateRequest.CacheScope != nil {
		cacheScope = templateRequest.GetCacheScope()
	}

	// Create the auth provider using the factory
	authProvider := auth.NewAuthProvider(cfg.GetFromImageRegistry())

	// TODO: Remove, temporary handling when version is not sent from the API
	version := templateRequest.GetVersion()
	if version == "" {
		version = templates.TemplateV2BetaVersion
	}

	ctx = featureflags.AddToContext(
		ctx,
		featureflags.TemplateContext(cfg.GetTemplateID()),
		featureflags.TeamContext(cfg.GetTeamID()),
	)

	kernelVersion := s.featureFlags.StringFlag(ctx, featureflags.BuildKernelVersion)
	firecrackerVersion := s.featureFlags.StringFlag(ctx, featureflags.BuildFirecrackerVersion)

	// Read once here, at the only place the per-team flag is read, so every boot in this
	// build agrees and what gets recorded is what was actually applied.
	cmdlineArgs, cmdlineErr := fc.ParseCmdlineArgs(s.featureFlags.StringFlag(ctx, featureflags.BuildKernelCmdlineArgs))
	if cmdlineErr == nil {
		cmdlineErr = fc.ValidateCmdlineArgs(cmdlineArgs)
	}
	if cmdlineErr != nil {
		// Fall back to the default command line rather than fail the build: a flag set
		// ahead of a deploy, or left set behind a rollback, should not stop a team
		// shipping templates.
		//
		// s.logger, not s.buildLogger: buildLogger feeds the caller-visible build log
		// stream, and a malformed internal flag is an operator concern the caller never
		// asked about. This is also the only record of what was rejected — the span
		// attribute and the counter both report what was APPLIED.
		s.logger.Warn(ctx, "rejected guest kernel cmdline args, using the default",
			zap.Error(cmdlineErr),
			logger.WithTemplateID(cfg.GetTemplateID()),
			logger.WithBuildID(cfg.GetBuildID()),
		)

		cmdlineArgs = nil
	}

	fcInfo, err := fcversion.New(firecrackerVersion)
	if err != nil {
		return nil, fmt.Errorf("invalid resolved firecracker version %q: %w", firecrackerVersion, err)
	}

	// Read once per build, so every layer boots with the same template.
	cpuTemplateResult := cpuTemplateNone
	var cpuTemplate *cputemplate.Template
	cpuTemplate, err = cputemplate.Parse([]byte(s.featureFlags.JSONFlag(ctx, featureflags.BuildCPUTemplate).JSONString()))
	if err != nil {
		// A malformed flag fails the build: falling back would silently boot at host frequency.
		// The flag and parse detail stay in the log and the counter, not the client's message.
		s.logger.Error(ctx, "invalid build CPU template flag",
			zap.String("flag", featureflags.BuildCPUTemplate.Key()),
			zap.Error(err),
			logger.WithTeamID(cfg.GetTeamID()),
			logger.WithTemplateID(cfg.GetTemplateID()),
			logger.WithBuildID(cfg.GetBuildID()),
		)
		childSpan.SetAttributes(attribute.String("env.cpu_template_result", cpuTemplateMalformed))
		buildCPUTemplate.Add(ctx, 1, metric.WithAttributes(
			attribute.String("template", ""),
			attribute.String("result", cpuTemplateMalformed),
		))

		return nil, status.Error(codes.Internal, "invalid build configuration, contact support")
	}
	if cpuTemplate != nil {
		// A template this Firecracker version or host architecture cannot apply falls back to
		// none, so one flag value can serve a mixed fleet during rollout. The span and counter
		// mark the build rejected; this log carries the reason.
		if err := cpuTemplate.Validate(fcInfo); err != nil {
			cpuTemplateErr := fmt.Errorf("firecracker %s: %w", firecrackerVersion, err)
			s.logger.Warn(ctx, "rejected build CPU template, using none",
				zap.Error(cpuTemplateErr),
				logger.WithTemplateID(cfg.GetTemplateID()),
				logger.WithBuildID(cfg.GetBuildID()),
			)

			cpuTemplate = nil
			cpuTemplateResult = cpuTemplateRejected
		} else {
			cpuTemplateResult = cpuTemplateApplied
		}
	}

	hugePages := fcInfo.HasHugePages()
	freePageReporting := fcInfo.HasFreePageReporting() && s.featureFlags.BoolFlag(ctx, featureflags.FreePageReportingFlag)
	freePageHinting := fcInfo.HasFreePageHinting() && featureflags.IsFreePageHintingEnabled(ctx, s.featureFlags)

	childSpan.SetAttributes(
		telemetry.WithTemplateID(cfg.GetTemplateID()),
		telemetry.WithBuildID(cfg.GetBuildID()),
		telemetry.WithKernelVersion(kernelVersion),
		telemetry.WithFirecrackerVersion(firecrackerVersion),
		attribute.String("env.start_cmd", cfg.GetStartCommand()),
		attribute.Int64("env.memory_mb", int64(cfg.GetMemoryMB())),
		attribute.Int64("env.vcpu_count", int64(cfg.GetVCpuCount())),
		attribute.Bool("env.huge_pages", hugePages),
		attribute.Bool("env.free_page_reporting", freePageReporting),
		attribute.Bool("env.free_page_hinting", freePageHinting),
		attribute.String("env.kernel_cmdline_args", fc.KernelArgs(cmdlineArgs).String()),
		attribute.String("env.cpu_template", cputemplate.AppliedDigest(cpuTemplate)),
		attribute.String("env.cpu_template_result", cpuTemplateResult),
	)

	buildCmdlineArgs.Add(ctx, 1, metric.WithAttributes(
		attribute.String("args", fc.KernelArgs(cmdlineArgs).String()),
	))

	buildCPUTemplate.Add(ctx, 1, metric.WithAttributes(
		attribute.String("template", cputemplate.AppliedDigest(cpuTemplate)),
		attribute.String("result", cpuTemplateResult),
	))

	freeDiskSizeMB := resolveFreeDiskSizeMB(cfg)

	template := config.TemplateConfig{
		Version:              version,
		TeamID:               cfg.GetTeamID(),
		TemplateID:           cfg.GetTemplateID(),
		CacheScope:           cacheScope,
		VCpuCount:            int64(cfg.GetVCpuCount()),
		MemoryMB:             int64(cfg.GetMemoryMB()),
		StartCmd:             cfg.GetStartCommand(),
		ReadyCmd:             cfg.GetReadyCommand(),
		DiskSizeMB:           int64(cfg.GetDiskSizeMB()),
		FreeDiskSizeMB:       freeDiskSizeMB,
		HugePages:            hugePages,
		FreePageReporting:    freePageReporting,
		FreePageHinting:      freePageHinting,
		FromImage:            cfg.GetFromImage(),
		FromTemplate:         cfg.GetFromTemplate(),
		RegistryAuthProvider: authProvider,
		Force:                cfg.Force,
		Steps:                cfg.GetSteps(),
		KernelVersion:        kernelVersion,
		FirecrackerVersion:   firecrackerVersion,
		CmdlineArgs:          cmdlineArgs,
		CPUTemplate:          cpuTemplate,
	}

	logs := buildlogger.NewLogEntryLogger()
	buildInfo, err := s.buildCache.Create(template.TeamID, metadata.BuildID, logs)
	if err != nil {
		return nil, fmt.Errorf("error while creating build cache: %w", err)
	}

	// LogEntryLogger is itself a zapcore.Core that captures every entry into
	// an in-memory slice; tee it with the regular build logger so logs go to
	// both destinations.
	core := zapcore.NewTee(logs, s.buildLogger.Detach(ctx).Core().
		With([]zap.Field{
			{Type: zapcore.StringType, Key: "teamID", String: template.TeamID},
			{Type: zapcore.StringType, Key: "envID", String: cfg.GetTemplateID()},
			{Type: zapcore.StringType, Key: "buildID", String: metadata.BuildID},
		}),
	)

	// Register child work before the foreground request releases its hold.
	buildDone := s.info.TrackWork()
	s.wg.Add(1)
	s.activeBuilds.Add(1)
	go func(ctx context.Context) {
		defer s.wg.Done()
		defer buildDone()
		defer s.activeBuilds.Add(-1)

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		ctx, buildSpan := tracer.Start(ctx, "template-background-build", trace.WithAttributes(
			telemetry.WithTemplateID(template.TemplateID),
			telemetry.WithBuildID(metadata.BuildID),
			telemetry.WithTeamID(template.TeamID),
		))
		defer buildSpan.End()

		defer func() {
			if r := recover(); r != nil {
				telemetry.ReportCriticalError(ctx, "recovered from panic in template build handler", nil, attribute.String("panic", fmt.Sprintf("%v", r)), telemetry.WithTemplateID(cfg.GetTemplateID()), telemetry.WithBuildID(cfg.GetBuildID()))
				buildInfo.SetFail(builderrors.UnwrapUserError(nil))
			}
		}()

		// Watch for build cancellation requests
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-buildInfo.Result.Done:
				res, _ := buildInfo.Result.Result()
				if res.Status == templatemanager.TemplateBuildState_Failed {
					cancel()
				}

				return
			}
		}()

		res, err := s.builder.Build(ctx, metadata, template, core)
		_ = core.Sync()
		if err != nil {
			userError := builderrors.UnwrapUserError(err)

			attrs := []attribute.KeyValue{
				telemetry.WithTemplateID(cfg.GetTemplateID()),
				telemetry.WithBuildID(cfg.GetBuildID()),
			}
			if userError.GetMessage() == builderrors.InternalErrorMessage {
				telemetry.ReportCriticalError(ctx, "error while building template", err, attrs...)
			} else {
				telemetry.ReportError(ctx, "error while building template", err, attrs...)
			}

			buildInfo.SetFail(userError)
		} else {
			buildInfo.SetSuccess(&templatemanager.TemplateBuildMetadata{
				RootfsSizeKey:      int32(res.RootfsSizeMB),
				EnvdVersionKey:     res.EnvdVersion,
				KernelVersion:      res.KernelVersion,
				FirecrackerVersion: res.FirecrackerVersion,
				SchedulingMetadata: res.SchedulingMetadata,
			})
			telemetry.ReportEvent(ctx, "Environment built")
		}
	}(context.WithoutCancel(ctx))

	return nil, nil
}

// Results of resolving the build CPU template flag, for the counter's result attribute.
const (
	cpuTemplateApplied  = "applied"
	cpuTemplateRejected = "rejected"
	cpuTemplateNone     = "none"
	// cpuTemplateMalformed is a flag value that does not parse; the build fails.
	cpuTemplateMalformed = "malformed"
)

func resolveFreeDiskSizeMB(cfg *templatemanager.TemplateConfig) int64 {
	if cfg.FreeDiskSizeMB != nil {
		return int64(cfg.GetFreeDiskSizeMB())
	}

	return int64(cfg.GetDiskSizeMB())
}
```

Flag defaults:

`packages/shared/pkg/featureflags/flags.go` lines 912-967 (of 1787)

```go
const (
	DefaultKernelVersion = "vmlinux-6.1.158"

	// DefaultEnvdVersion is the envd new template builds bake when neither the
	// build-envd-version flag nor DEFAULT_ENVD_VERSION says otherwise:
	// "promoted" selects the node-local promoted binary (HOST_ENVD_PATH), the
	// behavior every build has always had, so deployments without
	// LaunchDarkly (dev, self-host) are unaffected.
	DefaultEnvdVersion = "promoted"
)

// The Firecracker version per release line: legacy lines pin
// last-tag_short-SHA dev builds; e2b lines (vX.Y-<e2b-major>) pin releases
// published by the Publish fc-versions workflow.
// TODO: The short tag here has only 7 characters — the one from our build pipeline will likely have exactly 8 so this will break.
const (
	DefaultFirecrackerV1_10Version = "v1.10.1_30cbb07"
	DefaultFirecrackerV1_12Version = "v1.12.1_210cbac"
	DefaultFirecrackerV1_14Version = "v1.14.1_431f1fc"
	// The v1.14-0 release line. 0.2.0 introduces the in-place checkpoint's
	// balloon reporting-pause API; filesystem-only snapshots ship with every
	// e2b release from 0.1.0 — the per-feature floors live in fcversion.
	DefaultFirecrackerV1_14_0Version = "v1.14-0.2.0"
	// New template builds get the current release; existing builds keep
	// resolving within their own line below (cross-line upgrades are an
	// operator decision via the firecracker-versions flag, never a baked
	// default — the map invariant key == LDKey(value) enforces it).
	DefaultFirecrackerVersion = DefaultFirecrackerV1_14_0Version
)

var FirecrackerVersionMap = map[string]string{
	"v1.10":   DefaultFirecrackerV1_10Version,
	"v1.12":   DefaultFirecrackerV1_12Version,
	"v1.14":   DefaultFirecrackerV1_14Version,
	"v1.14-0": DefaultFirecrackerV1_14_0Version,
}

// BuildIoEngine Sync is used by default as there seems to be a bad interaction between Async and a lot of io operations.
var (
	BuildFirecrackerVersion = NewStringFlag("build-firecracker-version", env.GetEnv("DEFAULT_FIRECRACKER_VERSION", DefaultFirecrackerVersion))
	BuildKernelVersion      = NewStringFlag("build-kernel-version", env.GetEnv("DEFAULT_KERNEL_VERSION", DefaultKernelVersion))
	// BuildEnvdVersion selects which staged envd binary a template build bakes
	// into the rootfs — the envd counterpart of BuildKernelVersion /
	// BuildFirecrackerVersion, same default mechanism. "promoted" (the
	// fallback) is the node-local promoted binary; a concrete version id
	// (e.g. v0.7.0, or a git SHA while those age out) selects a staged binary
	// (the flat envd.<id> sibling or the release bucket's <id>/envd layout,
	// see build/core/envd.ResolveBuildBinary). A pinned target that is not
	// staged FAILS the build rather than silently baking a different envd —
	// feature gates key on the baked version, so a silent substitute
	// misgates. The build-site LD context carries template/team, so cohort
	// canaries come for free.
	BuildEnvdVersion = NewStringFlag("build-envd-version", env.GetEnv("DEFAULT_ENVD_VERSION", DefaultEnvdVersion))
	BuildIoEngine    = NewStringFlag("build-io-engine", "Sync")

	// BuildKernelCmdlineArgs supplies extra guest kernel command line parameters at
```

`packages/shared/pkg/featureflags/flags.go` lines 1424-1460 (of 1787)

```go
func ResolveFirecrackerVersion(ctx context.Context, ff *Client, buildVersion string) string {
	info, err := fcversion.New(buildVersion)
	if err != nil {
		recordFirecrackerFallback(ctx, "parse_error", "")

		return buildVersion
	}

	key, ok := info.LDKey()
	if !ok {
		recordFirecrackerFallback(ctx, "no_ld_key", "")

		return buildVersion
	}

	versions := ff.JSONFlag(ctx, FirecrackerVersions).AsValueMap()

	if resolved, ok := versions.Get(key).AsOptionalString().Get(); ok {
		// An empty map value would blank the binary path fleet-wide; serve
		// the stored version instead and make the misconfiguration loud.
		if resolved == "" {
			recordFirecrackerFallback(ctx, "empty_value", key)

			return buildVersion
		}
		recordFirecrackerResolved(ctx, key, resolved)

		return resolved
	}

	recordFirecrackerFallback(ctx, "key_absent", key)

	return buildVersion
}

// ResolveEnvdUpgrade decides whether a resuming sandbox's envd should be swapped
// for a newer node-local build, per EnvdUpgradeTargetFlag, and returns the local
```

### 6.2 `fromImage`: accepted references, auth, insecure / localhost registries

(Commentary) There is no `insecure`, `PlainHTTP` or `localhost` handling anywhere in the build's OCI code
(grep of `packages/orchestrator/pkg/template` for `insecure|PlainHTTP|localhost` in the pull path finds
nothing). The reference is parsed with go-containerregistry `name.ParseReference` (default options) and
pulled with `remote.Image` **from the orchestrator host process** (not from inside a VM). Plain HTTP is
therefore decided solely by go-containerregistry v0.21.7's `Registry.Scheme()`: `http` for
`localhost:<port>`, `*.localhost[:port]`, `127.0.0.1[:port]`, `::1`, and RFC1918 IPs
(10/8, 172.16/12, 192.168/16); everything else is https (its `Ping` also falls back from https to http
only for those). Docker Hub refs (`name.DefaultRegistry` = `index.docker.io`) with no auth go through the
optional pull-through proxy (`DOCKERHUB_REMOTE_REPOSITORY_URL`); if that env is empty a direct
`remote.Image` pull is used. Auth: AWS ECR keys, GCP service-account JSON, or username/password
(`authn.Basic`). The image must match `linux/<TARGET_ARCH>`. Base rootfs size limit flag
`build-base-rootfs-size-limit-mb` default 25000.

`packages/orchestrator/pkg/template/build/core/oci/oci.go` lines 1-156 (of 481)

```go
//go:build linux

package oci

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/google/go-containerregistry/pkg/name"
	containerregistry "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/moby/go-archive"
	"github.com/moby/go-archive/chrootarchive"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/core/filesystem"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/core/oci/auth"
	"github.com/e2b-dev/infra/packages/shared/pkg/dockerhub"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/units"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

var tracer = otel.Tracer("github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/core/oci")

// ImageTooLargeError is returned when the uncompressed Docker image exceeds the maximum filesystem size.
type ImageTooLargeError struct {
	ImageSize int64 // actual uncompressed size in bytes, 0 if unknown
	MaxSize   int64 // maximum filesystem size in bytes
}

func (e *ImageTooLargeError) Error() string {
	if e.ImageSize > 0 {
		return fmt.Sprintf(
			"the uncompressed Docker image size (%s) exceeds the maximum filesystem size (%s). "+
				"Please reduce your Docker image size (e.g., use a smaller base image, multi-stage builds, or remove unnecessary files)",
			humanize.Bytes(uint64(e.ImageSize)),
			humanize.Bytes(uint64(e.MaxSize)),
		)
	}

	return fmt.Sprintf(
		"the Docker image is too large for the maximum filesystem size of %s. "+
			"Please reduce your Docker image size (e.g., use a smaller base image, multi-stage builds, or remove unnecessary files)",
		humanize.Bytes(uint64(e.MaxSize)),
	)
}

// DefaultPlatform returns the OCI platform for image pulls, respecting TARGET_ARCH.
func DefaultPlatform() containerregistry.Platform {
	return containerregistry.Platform{
		OS:           "linux",
		Architecture: utils.TargetArch(),
	}
}

// wrapImagePullError converts technical Docker registry errors into user-friendly messages.
func wrapImagePullError(ctx context.Context, err error, imageRef string) error {
	if err == nil {
		return nil
	}

	logger.L().Warn(ctx, "failed to pull image", zap.String("image_ref", imageRef), zap.Error(err))

	// Check for transport errors with specific error codes from the registry API
	if transportErr, ok := errors.AsType[*transport.Error](err); ok {
		for _, e := range transportErr.Errors {
			switch e.Code {
			case transport.ManifestUnknownErrorCode:
				return fmt.Errorf("image '%s' not found: the image or tag does not exist in the registry", imageRef)
			case transport.NameUnknownErrorCode:
				return fmt.Errorf("repository '%s' not found: verify the image name is correct", imageRef)
			case transport.UnauthorizedErrorCode:
				return fmt.Errorf("access denied to '%s': authentication required or insufficient permissions", imageRef)
			case transport.DeniedErrorCode:
				return fmt.Errorf("access denied to '%s': you don't have permission to pull this image", imageRef)
			}
		}

		if transportErr.StatusCode != 0 {
			return fmt.Errorf("failed to pull image '%s': registry returned status code %d", imageRef, transportErr.StatusCode)
		}
	}

	return fmt.Errorf("failed to pull image '%s': unable to retrieve image from registry", imageRef)
}

func GetPublicImage(ctx context.Context, dockerhubRepository dockerhub.RemoteRepository, tag string, authProvider auth.RegistryAuthProvider) (containerregistry.Image, error) {
	ctx, span := tracer.Start(ctx, "pull-public-docker-image")
	defer span.End()

	ref, err := name.ParseReference(tag)
	if err != nil {
		return nil, fmt.Errorf("invalid image reference '%s': %w", tag, err)
	}

	platform := DefaultPlatform()

	// When no auth provider is provided and the image is from the default registry
	// use docker remote repository proxy with cached images
	if authProvider == nil && ref.Context().RegistryStr() == name.DefaultRegistry {
		img, err := dockerhubRepository.GetImage(ctx, tag, platform)
		if err != nil {
			return nil, wrapImagePullError(ctx, err, tag)
		}

		telemetry.ReportEvent(ctx, "pulled public image through docker remote repository proxy")

		err = verifyImagePlatform(ctx, img, platform, tag)
		if err != nil {
			return nil, err
		}

		return img, nil
	}

	// Build options
	opts := []remote.Option{remote.WithPlatform(platform)}

	// Use the auth provider if provided
	if authProvider != nil {
		authOption, err := authProvider.GetAuthOption(ctx)
		if err != nil {
			return nil, fmt.Errorf("error getting auth option: %w", err)
		}
		if authOption != nil {
			opts = append(opts, authOption)
		}
	}

	img, err := remote.Image(ref, opts...)
	if err != nil {
		return nil, wrapImagePullError(ctx, err, tag)
	}

	telemetry.ReportEvent(ctx, "pulled public image")

	err = verifyImagePlatform(ctx, img, platform, tag)
	if err != nil {
		return nil, err
	}

	return img, nil
}
```

`packages/orchestrator/pkg/template/build/core/oci/auth/auth.go` lines 1-33 (of 33)

```go
package auth

import (
	"context"

	"github.com/google/go-containerregistry/pkg/v1/remote"

	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
)

// RegistryAuthProvider is an interface for different registry authentication providers
type RegistryAuthProvider interface {
	// GetAuthOption returns the remote.Option for authentication
	GetAuthOption(ctx context.Context) (remote.Option, error)
}

// NewAuthProvider is a factory function that creates the appropriate auth provider
func NewAuthProvider(registry *templatemanager.FromImageRegistry) RegistryAuthProvider {
	if registry == nil {
		return nil
	}

	switch auth := registry.GetType().(type) {
	case *templatemanager.FromImageRegistry_Aws:
		return NewAWSAuthProvider(auth.Aws)
	case *templatemanager.FromImageRegistry_Gcp:
		return NewGCPAuthProvider(auth.Gcp)
	case *templatemanager.FromImageRegistry_General:
		return NewGeneralAuthProvider(auth.General)
	default:
		return nil
	}
}
```

`packages/orchestrator/pkg/template/build/core/oci/auth/general.go` lines 1-30 (of 30)

```go
package auth

import (
	"context"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
)

// GeneralAuthProvider implements authentication for registries with username/password
type GeneralAuthProvider struct {
	registry *templatemanager.GeneralRegistry
}

// NewGeneralAuthProvider creates a new general auth provider
func NewGeneralAuthProvider(registry *templatemanager.GeneralRegistry) *GeneralAuthProvider {
	return &GeneralAuthProvider{
		registry: registry,
	}
}

// GetAuthOption returns the authentication option for general registries
func (p *GeneralAuthProvider) GetAuthOption(context.Context) (remote.Option, error) {
	return remote.WithAuth(&authn.Basic{
		Username: p.registry.GetUsername(),
		Password: p.registry.GetPassword(),
	}), nil
}
```

`packages/orchestrator/pkg/template/build/core/oci/auth/aws.go` lines 1-74 (of 74)

```go
package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
)

// AWSAuthProvider implements authentication for AWS ECR
type AWSAuthProvider struct {
	registry *templatemanager.AWSRegistry
}

// NewAWSAuthProvider creates a new AWS auth provider
func NewAWSAuthProvider(registry *templatemanager.AWSRegistry) *AWSAuthProvider {
	return &AWSAuthProvider{
		registry: registry,
	}
}

// GetAuthOption returns the authentication option for AWS ECR
func (p *AWSAuthProvider) GetAuthOption(ctx context.Context) (remote.Option, error) {
	// Load AWS configuration with the provided credentials
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(p.registry.GetAwsRegion()),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			p.registry.GetAwsAccessKeyId(),
			p.registry.GetAwsSecretAccessKey(),
			"",
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Create ECR client and get authorization token
	ecrClient := ecr.NewFromConfig(cfg)
	token, err := ecrClient.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return nil, fmt.Errorf("failed to get ECR authorization token: %w", err)
	}

	if len(token.AuthorizationData) == 0 {
		return nil, errors.New("no ECR authorization data returned")
	}

	// Decode the authorization token
	authData := token.AuthorizationData[0]
	decodedToken, err := base64.StdEncoding.DecodeString(*authData.AuthorizationToken)
	if err != nil {
		return nil, fmt.Errorf("failed to decode ECR token: %w", err)
	}

	// Parse the token (format is username:password)
	parts := strings.SplitN(string(decodedToken), ":", 2)
	if len(parts) != 2 {
		return nil, errors.New("invalid ECR token format")
	}

	return remote.WithAuth(&authn.Basic{
		Username: parts[0],
		Password: parts[1],
	}), nil
}
```

`packages/orchestrator/pkg/template/build/core/oci/auth/gcp.go` lines 1-30 (of 30)

```go
package auth

import (
	"context"

	"github.com/google/go-containerregistry/pkg/v1/google"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
)

// GCPAuthProvider implements authentication for Google Container Registry
type GCPAuthProvider struct {
	registry *templatemanager.GCPRegistry
}

// NewGCPAuthProvider creates a new GCP auth provider
func NewGCPAuthProvider(registry *templatemanager.GCPRegistry) *GCPAuthProvider {
	return &GCPAuthProvider{
		registry: registry,
	}
}

// GetAuthOption returns the authentication option for GCP
func (p *GCPAuthProvider) GetAuthOption(context.Context) (remote.Option, error) {
	// Create authenticator using the service account JSON
	authenticator := google.NewJSONKeyAuthenticator(p.registry.GetServiceAccountJson())

	return remote.WithAuth(authenticator), nil
}
```

`packages/shared/pkg/dockerhub/repository.go` lines 14-60 (of 72)

```go

type RemoteRepositoryProvider string

const (
	GCPStorageProvider   RemoteRepositoryProvider = "GCP_REMOTE_REPOSITORY"
	AWSStorageProvider   RemoteRepositoryProvider = "AWS_ECR"
	AzureStorageProvider RemoteRepositoryProvider = "AZURE_ACR"
	LocalStorageProvider RemoteRepositoryProvider = "Local"

	DefaultRegistryProvider RemoteRepositoryProvider = GCPStorageProvider

	storageProviderEnv         = "DOCKERHUB_REMOTE_REPOSITORY_PROVIDER"
	storageRemoteRepositoryURL = "DOCKERHUB_REMOTE_REPOSITORY_URL"

	setupTimeout = 10 * time.Second
)

type RemoteRepository interface {
	GetImage(ctx context.Context, tag string, platform containerregistry.Platform) (containerregistry.Image, error)
	Close() error
}

func GetRemoteRepository(ctx context.Context) (RemoteRepository, error) {
	provider := RemoteRepositoryProvider(env.GetEnv(storageProviderEnv, string(DefaultRegistryProvider)))

	dockerRemoteRepositoryURL := env.GetEnv(storageRemoteRepositoryURL, "")
	if dockerRemoteRepositoryURL == "" {
		return NewNoopRemoteRepository(), nil
	}

	setupCtx, setupCtxCancel := context.WithTimeout(ctx, setupTimeout)
	defer setupCtxCancel()

	switch provider {
	case AWSStorageProvider:
		return NewAWSRemoteRepository(setupCtx, dockerRemoteRepositoryURL)
	case GCPStorageProvider:
		return NewGCPRemoteRepository(setupCtx, dockerRemoteRepositoryURL)
	case AzureStorageProvider:
		return NewAzureRemoteRepository(setupCtx, dockerRemoteRepositoryURL)
	case LocalStorageProvider:
		return NewNoopRemoteRepository(), nil
	}

	return nil, fmt.Errorf("unknown dockerhub remote repository provider: %s", provider)
}

```

`packages/shared/pkg/dockerhub/repository_noop.go` lines 1-34 (of 34)

```go
package dockerhub

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	containerregistry "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

type NoopRemoteRepository struct{}

func NewNoopRemoteRepository() *NoopRemoteRepository {
	return &NoopRemoteRepository{}
}

func (n *NoopRemoteRepository) GetImage(_ context.Context, tag string, platform containerregistry.Platform) (containerregistry.Image, error) {
	ref, err := name.ParseReference(tag)
	if err != nil {
		return nil, fmt.Errorf("invalid image reference: %w", err)
	}

	img, err := remote.Image(ref, remote.WithPlatform(platform))
	if err != nil {
		return nil, fmt.Errorf("error pulling image: %w", err)
	}

	return img, nil
}

func (n *NoopRemoteRepository) Close() error {
	return nil
}
```

`github.com/google/go-containerregistry@v0.21.7/pkg/name/registry.go` lines 25-108

```go
)

// Detect more complex forms of localhost references.
var reLocal = regexp.MustCompile(`.*\.localhost(?::\d{1,5})?$`)

// Detect the loopback IP (127.0.0.1)
var reLoopback = regexp.MustCompile(`^127\.0\.0\.1(?::\d{1,5})?$`)

// Detect the loopback IPV6 (::1)
var reipv6Loopback = regexp.MustCompile(`^(::1|\[::1\](?::\d{1,5})?)$`)

// Registry stores a docker registry name in a structured form.
type Registry struct {
	insecure bool
	registry string
}

var _ encoding.TextMarshaler = (*Registry)(nil)
var _ encoding.TextUnmarshaler = (*Registry)(nil)
var _ json.Marshaler = (*Registry)(nil)
var _ json.Unmarshaler = (*Registry)(nil)

// RegistryStr returns the registry component of the Registry.
func (r Registry) RegistryStr() string {
	return r.registry
}

// Name returns the name from which the Registry was derived.
func (r Registry) Name() string {
	return r.RegistryStr()
}

func (r Registry) String() string {
	return r.Name()
}

// Repo returns a Repository in the Registry with the given name.
func (r Registry) Repo(repo ...string) Repository {
	return Repository{Registry: r, repository: path.Join(repo...)}
}

// Scope returns the scope required to access the registry.
func (r Registry) Scope(string) string {
	// The only resource under 'registry' is 'catalog'. http://goo.gl/N9cN9Z
	return "registry:catalog:*"
}

func (r Registry) isRFC1918() bool {
	ipStr := strings.Split(r.Name(), ":")[0]
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		_, block, _ := net.ParseCIDR(cidr)
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// Scheme returns https scheme for all the endpoints except localhost or when explicitly defined.
func (r Registry) Scheme() string {
	if r.insecure {
		return "http"
	}
	if r.isRFC1918() {
		return "http"
	}
	if strings.HasPrefix(r.Name(), "localhost:") {
		return "http"
	}
	if reLocal.MatchString(r.Name()) {
		return "http"
	}
	if reLoopback.MatchString(r.Name()) {
		return "http"
	}
	if reipv6Loopback.MatchString(r.Name()) {
		return "http"
	}
	return "https"
}
```

`packages/shared/pkg/featureflags/flags.go` lines 642-642 (of 1787)

```go
	BuildBaseRootfsSizeLimitMB = NewIntFlag("build-base-rootfs-size-limit-mb", 25000)
```

### 6.3 Steps, user and shell

Step `type` is upper-cased and must be one of `ADD`/`COPY`, `RUN`, `USER`, `WORKDIR`, `ENV`/`ARG`.
`RUN` args are `[command, optional_user]`. Every command runs as `/bin/bash -l -c <command>` via envd
(§4.1). The base-image build context starts as user `root`, no workdir, empty env.

`packages/orchestrator/pkg/template/build/commands/executor.go` lines 47-74 (of 114)

```go
func (ce *CommandExecutor) getCommand(
	step *templatemanager.TemplateStep,
) (Command, error) {
	cmdType := strings.ToUpper(step.GetType())

	var cmd Command
	switch cmdType {
	case "ADD", "COPY":
		cmd = &Copy{
			FilesStorage: ce.buildStorage,
			CacheScope:   ce.CacheScope,
		}
	case "RUN":
		cmd = &Run{}
	case "USER":
		cmd = &User{}
	case "WORKDIR":
		cmd = &Workdir{}
	case "ENV", "ARG":
		cmd = &Env{}
	}

	if cmd == nil {
		return nil, fmt.Errorf("command type %s is not implemented", cmdType)
	}

	return cmd, nil
}
```

`packages/orchestrator/pkg/template/build/commands/run.go` lines 22-62 (of 62)

```go

func (r *Run) Execute(
	ctx context.Context,
	logger logger.Logger,
	lvl zapcore.Level,
	proxy *proxy.SandboxProxy,
	sandboxID string,
	prefix string,
	step *templatemanager.TemplateStep,
	cmdMetadata metadata.Context,
) (metadata.Context, error) {
	args := step.GetArgs()
	// args: [command optional_user]
	if len(args) < 1 {
		return metadata.Context{}, errors.New("RUN requires command argument")
	}

	originalMetadata := cmdMetadata

	// If a custom command user is specified, use it
	if len(args) >= 2 {
		cmdMetadata.User = args[1]
	}

	cmd := args[0]
	err := sandboxtools.RunCommandWithLogger(
		ctx,
		proxy,
		logger,
		lvl,
		prefix,
		sandboxID,
		cmd,
		cmdMetadata,
	)
	if err != nil {
		return metadata.Context{}, fmt.Errorf("failed to run command '%s': %w", cmd, err)
	}

	return originalMetadata, nil
}
```

`packages/orchestrator/pkg/template/build/phases/base/builder.go` lines 44-48 (of 381)

```go
	baseLayerTimeout = 10 * time.Minute

	defaultUser = "root"
)

```

`packages/orchestrator/pkg/template/build/phases/base/builder.go` lines 307-345 (of 381)

```go
	switch {
	case bb.Config.FromTemplate != nil:
		sourceMeta := metadata.FromTemplate{
			Alias:   bb.Config.FromTemplate.GetAlias(),
			BuildID: bb.Config.FromTemplate.GetBuildID(),
		}

		// If the template is built from another template, use its metadata
		tm, err := bb.index.Cached(ctx, bb.Config.FromTemplate.GetBuildID())
		if err != nil {
			if errors.Is(err, storage.ErrObjectNotExist) {
				return phases.LayerResult{}, phases.NewPhaseBuildError(bb.Metadata(), errors.New("error getting base template, you may need to rebuild it first"))
			}

			return phases.LayerResult{}, fmt.Errorf("error getting base template: %w", err)
		}

		// From template is always cached, never needs to be built
		return phases.LayerResult{
			Metadata: tm.BasedOn(sourceMeta),
			Hash:     hash,
			Cached:   true,
		}, nil
	default:
		cmdMeta := metadata.Context{
			User:    defaultUser,
			WorkDir: nil,
			EnvVars: make(map[string]string),
		}

		meta := metadata.Template{
			Version: metadata.CurrentVersion,
			Template: metadata.TemplateMetadata{
				BuildID:            uuid.New().String(),
				KernelVersion:      bb.Config.KernelVersion,
				FirecrackerVersion: bb.Config.FirecrackerVersion,
			},
			Context:      cmdMeta,
			FromImage:    &bb.Config.FromImage,
```

`packages/orchestrator/pkg/template/metadata/template_metadata.go` lines 61-65 (of 450)

```go
type Context struct {
	User    string            `json:"user,omitempty"`
	WorkDir *string           `json:"workdir,omitempty"`
	EnvVars map[string]string `json:"env_vars,omitempty"`
}
```

### 6.4 startCommand / readyCommand

(Commentary from the quote) Run in the finalize phase after the configuration script, with the
build's final context (user/workdir/env from the last USER/WORKDIR/ENV step; `root` if none),
through the same `/bin/bash -l -c` path. The start command runs in the background; the ready command
is retried every 2 s until exit code 0, with a 10 min overall timeout. Default ready command: `sleep 0`
when there is no start command, otherwise `sleep 20` (120 for three hard-coded template IDs). If the
start command exits non-zero before ready succeeds the build fails. After ready succeeds the VM is
snapshotted with the start command still running.

`packages/orchestrator/pkg/template/build/phases/finalize/ready.go` lines 1-86 (of 86)

```go
//go:build linux

package finalize

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap/zapcore"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/sandboxtools"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

const (
	defaultReadyWait = 20 * time.Second

	readyCommandRetryInterval = 2 * time.Second
	readyCommandTimeout       = 10 * time.Minute
)

func (ppb *PostProcessingBuilder) runReadyCommand(
	ctx context.Context,
	userLogger logger.Logger,
	sandboxID string,
	readyCmd string,
	cmdMetadata metadata.Context,
) error {
	ctx, span := tracer.Start(ctx, "run-ready-command")
	defer span.End()

	userLogger.Info(ctx, fmt.Sprintf("Waiting for template to be ready: %s", readyCmd))

	startTime := time.Now()
	ctx, cancel := context.WithTimeout(ctx, readyCommandTimeout)
	defer cancel()

	// Start the ready check
	for {
		err := sandboxtools.RunCommandWithLogger(
			ctx,
			ppb.proxy,
			userLogger,
			zapcore.DebugLevel,
			"ready",
			sandboxID,
			readyCmd,
			cmdMetadata,
		)

		if err == nil {
			userLogger.Info(ctx, "Template is ready")

			return nil
		}

		userLogger.Debug(ctx, fmt.Sprintf("Template is not ready: %v", err))

		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("ready command timed out after %s", time.Since(startTime))
			}
			// Template is ready, the start command finished before the ready command
			userLogger.Info(ctx, "Template is ready")

			return nil
		case <-time.After(readyCommandRetryInterval):
			// Wait for readyCommandRetryInterval time before retrying the ready command
		}
	}
}

func GetDefaultReadyCommand(templateID string) string {
	// HACK: This is a temporary fix for a customer that needs a bigger time to start the command.
	// TODO: Remove this after we can add customizable wait time for building templates.
	// TODO: Make this user configurable, with health check too
	if templateID == "zegbt9dl3l2ixqem82mm" || templateID == "ot5bidkk3j2so2j02uuz" || templateID == "0zeou1s7agaytqitvmzc" {
		return fmt.Sprintf("sleep %d", int((120 * time.Second).Seconds()))
	}

	return fmt.Sprintf("sleep %d", int(defaultReadyWait.Seconds()))
}
```

`packages/orchestrator/pkg/template/build/phases/finalize/builder.go` lines 104-125 (of 381)

```go
	result := sourceLayer.Metadata

	// If the start/ready commands are set,
	// use them instead of start metadata from the template it is built from.
	if ppb.Config.StartCmd != "" || ppb.Config.ReadyCmd != "" {
		result.Start = &metadata.Start{
			StartCmd: ppb.Config.StartCmd,
			ReadyCmd: ppb.Config.ReadyCmd,
			Context:  result.Context,
		}
	}

	// The final template is the one from the configuration
	result.Template = metadata.TemplateMetadata{
		BuildID:            ppb.Template.BuildID,
		KernelVersion:      ppb.Config.KernelVersion,
		FirecrackerVersion: ppb.Config.FirecrackerVersion,
	}

	// Stamped from the same config as the kernel version above, because it describes
	// the same thing: how this template's kernel was booted. From here the
	// copy-constructors carry it, so every later pause of this lineage keeps it and a
```

`packages/orchestrator/pkg/template/build/phases/finalize/builder.go` lines 290-381 (of 381)

```go
		}()

		// Run configuration script
		configCtx, configCancel := context.WithTimeout(ctx, configurationTimeout)
		defer configCancel()
		err := runConfiguration(
			configCtx,
			userLogger,
			ppb.BuildContext,
			ppb.proxy,
			sbx.Runtime.SandboxID,
		)
		if err != nil {
			return metadata.Template{}, phases.NewPhaseBuildError(ppb.Metadata(), fmt.Errorf("configuration script failed: %w", err))
		}

		if meta.Start == nil {
			return meta, nil
		}

		// Start command
		commandsCtx, commandsCancel := context.WithCancel(ctx)
		defer commandsCancel()

		var startCmdRun errgroup.Group
		startCmdConfirm := make(chan struct{})
		if meta.Start.StartCmd != "" {
			userLogger.Info(ctx, fmt.Sprintf("Running start command: %s", meta.Start.StartCmd))
			startCmdRun.Go(func() error {
				err := sandboxtools.RunCommandWithConfirmation(
					commandsCtx,
					ppb.proxy,
					userLogger,
					zapcore.InfoLevel,
					"start",
					sbx.Runtime.SandboxID,
					meta.Start.StartCmd,
					meta.Start.Context,
					startCmdConfirm,
				)
				// If the ctx is canceled, the ready command succeeded and no start command await is necessary.
				if err != nil && !errors.Is(err, context.Canceled) {
					// Cancel the ready command context, so the ready command does not wait anymore if an error occurs.
					commandsCancel()

					return fmt.Errorf("error running start command: %w", err)
				}

				return nil
			})
		} else {
			// If no start command is defined, we still need to confirm that the start command has started.
			close(startCmdConfirm)
		}

		// Ready command
		readyCmd := meta.Start.ReadyCmd
		if readyCmd == "" {
			if meta.Start.StartCmd == "" {
				readyCmd = "sleep 0"
			} else {
				readyCmd = GetDefaultReadyCommand(ppb.Config.TemplateID)
			}
		}
		err = ppb.runReadyCommand(
			commandsCtx,
			userLogger,
			sbx.Runtime.SandboxID,
			readyCmd,
			meta.Start.Context,
		)
		if err != nil {
			return metadata.Template{}, phases.NewPhaseBuildError(ppb.Metadata(), fmt.Errorf("ready command failed: %w", err))
		}

		// Wait for the start command to start executing.
		select {
		case <-ctx.Done():
			return metadata.Template{}, phases.NewPhaseBuildError(ppb.Metadata(), fmt.Errorf("waiting for start command failed: %w", commandsCtx.Err()))
		case <-startCmdConfirm:
		}
		// Cancel the start command context (it's running in the background anyway).
		// If it has already finished, check the error.
		commandsCancel()
		err = startCmdRun.Wait()
		if err != nil {
			return metadata.Template{}, phases.NewPhaseBuildError(ppb.Metadata(), fmt.Errorf("start command failed: %w", err))
		}

		return meta, nil
	}
}
```

### 6.5 Build status and metadata

`TemplateBuildStatus` reads an in-memory per-build cache (10 min TTL, refreshed on each `Get`, so it
must be polled at least every 10 min; it answers an error once expired). While running it returns
`Building` with no metadata; on success `Completed` + `TemplateBuildMetadata`; on failure `Failed` +
`reason`. Logs: max 100 entries per call, default window last 24 h, `offset` counts entries after
level/time filtering, `level` is a minimum.

`packages/orchestrator/pkg/template/server/template_status.go` lines 1-103 (of 103)

```go
//go:build linux

package server

import (
	"context"
	"fmt"
	"slices"
	"time"

	template_manager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
)

const (
	maxLogEntriesPerRequest = uint32(100)
	defaultTimeRange        = 24 * time.Hour

	defaultDirection = template_manager.LogsDirection_Forward
)

func (s *ServerStore) TemplateBuildStatus(ctx context.Context, in *template_manager.TemplateStatusRequest) (*template_manager.TemplateBuildStatusResponse, error) {
	_, span := tracer.Start(ctx, "template-build-status-request")
	defer span.End()

	buildInfo, err := s.buildCache.Get(in.GetBuildID())
	if err != nil {
		return nil, fmt.Errorf("error while getting build info, maybe already expired: %w", err)
	}

	limit := maxLogEntriesPerRequest
	if in.Limit != nil && in.GetLimit() < maxLogEntriesPerRequest {
		limit = in.GetLimit()
	}

	direction := defaultDirection
	if in.GetDirection() == template_manager.LogsDirection_Backward {
		direction = template_manager.LogsDirection_Backward
	}

	start, end := time.Now().Add(-defaultTimeRange), time.Now()
	if s := in.GetStart(); s != nil {
		start = s.AsTime()
	}
	if e := in.GetEnd(); e != nil {
		end = e.AsTime()
	}

	logLines := buildInfo.GetLogs()

	// Keep response ordering aligned with persistent log mapping.
	slices.SortStableFunc(logLines, func(a, b *template_manager.TemplateBuildLogEntry) int {
		if direction == template_manager.LogsDirection_Backward {
			return b.GetTimestamp().AsTime().Compare(a.GetTimestamp().AsTime())
		}

		return a.GetTimestamp().AsTime().Compare(b.GetTimestamp().AsTime())
	})

	logEntries := make([]*template_manager.TemplateBuildLogEntry, 0)
	logsCrawled := int32(0)
	for _, entry := range logLines {
		// Skip entries that are below the specified level
		if entry.GetLevel().Number() < in.GetLevel().Number() {
			continue
		}

		if entry.GetTimestamp().AsTime().Before(start) {
			continue
		}

		if entry.GetTimestamp().AsTime().After(end) {
			continue
		}

		logsCrawled++
		if logsCrawled <= in.GetOffset() {
			continue
		}

		if uint32(len(logEntries)) >= limit {
			break
		}

		logEntries = append(logEntries, entry)
	}

	result := buildInfo.GetResult()
	if result == nil {
		return &template_manager.TemplateBuildStatusResponse{
			Status:     template_manager.TemplateBuildState_Building,
			Reason:     nil,
			Metadata:   nil,
			LogEntries: logEntries,
		}, nil
	}

	return &template_manager.TemplateBuildStatusResponse{
		Status:     result.Status,
		Reason:     result.Reason,
		Metadata:   result.Metadata,
		LogEntries: logEntries,
	}, nil
}
```

`packages/orchestrator/pkg/template/cache/build_cache.go` lines 19-21 (of 168)

```go
const (
	buildInfoExpiration = time.Minute * 10 // 10 minutes
)
```

`packages/orchestrator/pkg/template/cache/build_cache.go` lines 148-164 (of 168)

```go
func (c *BuildCache) Create(teamID string, buildID string, logs *buildlogger.LogEntryLogger) (*BuildInfo, error) {
	info := &BuildInfo{
		TeamID: teamID,
		logs:   logs,
		Result: utils.NewSetOnce[BuildInfoResult](),
	}

	_, found := c.cache.GetOrSet(buildID, info,
		ttlcache.WithTTL[string, *BuildInfo](buildInfoExpiration),
		ttlcache.WithDisableTouchOnHit[string, *BuildInfo](),
	)
	if found {
		return nil, fmt.Errorf("build %s already exists in cache", buildID)
	}

	return info, nil
}
```

`packages/orchestrator/pkg/template/build/builder.go` lines 98-110 (of 518)

```go
type Result struct {
	EnvdVersion        string
	KernelVersion      string
	FirecrackerVersion string
	RootfsSizeMB       int64
	SchedulingMetadata *orchestratorgrpc.SchedulingMetadata
}

// Build builds the template, uploads it to storage and returns the result metadata.
// It works the following:
// 1. Get docker image from the remote repository
// 2. Inject new file layers with the required setup for hostname, dns, envd service configuration, basic provisioning script that is run before most of VM services
// 3. Extract ext4 filesystem
```

`packages/orchestrator/pkg/template/build/builder.go` lines 440-462 (of 518)

```go
	// Ensure the base layer is uploaded before getting the rootfs size
	err = bc.UploadErrGroup.Wait()
	if err != nil {
		return nil, fmt.Errorf("error waiting for layers upload: %w", err)
	}

	// Get the base rootfs size from the template files
	// This is the size of the rootfs after provisioning and before building the layers
	// (as they don't change the rootfs size)
	rootfsSize, err := getRootfsSize(ctx, builder.templateStorage, storage.Paths{BuildID: lastLayerResult.Metadata.Template.BuildID})
	if err != nil {
		return nil, fmt.Errorf("error getting rootfs size: %w", err)
	}
	logger.L().Info(ctx, "rootfs size", zap.Uint64("size", rootfsSize))

	return &Result{
		EnvdVersion:        bc.EnvdVersion,
		KernelVersion:      bc.Config.KernelVersion,
		FirecrackerVersion: bc.Config.FirecrackerVersion,
		RootfsSizeMB:       units.BytesToMB(int64(rootfsSize)),
		SchedulingMetadata: templateSchedulingMetadata(ctx, builder.templateCache, lastLayerResult.Metadata.Template.BuildID),
	}, nil
}
```

### 6.6 Version string formats

- kernel: `vmlinux-<x.y.z>`, current default `vmlinux-6.1.158` (§6.1 flags quote).
- firecracker: three formats (legacy `vX.Y.Z_<commit>`, bare `vX.Y.Z`, e2b `vX.Y-<a.b.c>`), parsed by `fcversion.New` (§3.2 quote). Current default `v1.14-0.2.0`. At sandbox start the orchestrator re-resolves the declared version through the `firecracker-versions` flag (`ResolveFirecrackerVersion`) and echoes the result in `SandboxCreateResponse.resolved_firecracker_version`.
- envd: the trimmed stdout of `envd -version`; envd's own version constant is shown below (no `v` prefix). The API compares with `golang.org/x/mod/semver` after prefixing `v` (min 0.5.0 for snapshots/pause/fork, 0.2.0 for secure).

`packages/orchestrator/pkg/template/build/core/envd/envd.go` lines 1-23 (of 23)

```go
package envd

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

func GetEnvdVersion(ctx context.Context, envdPath string) (string, error) {
	cmd := exec.CommandContext(ctx, envdPath, "-version")
	// A binary on a broken mount can hang in uninterruptible I/O where even
	// the ctx-deadline SIGKILL doesn't reap it; WaitDelay makes Output return
	// once the ctx is done anyway instead of waiting on the corpse forever.
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("error while getting envd version: %w", err)
	}

	return strings.TrimSpace(string(out)), nil
}
```

`packages/envd/pkg/version.go` lines 1-3 (of 3)

```go
package pkg

var Version = "0.9.0" // x-release-please-version
```

`packages/shared/pkg/utils/version.go` lines 1-72 (of 72)

```go
package utils

import (
	"fmt"

	"golang.org/x/mod/semver"
)

const MinEnvdVersionForSnapshot = "0.5.0"

// MinEnvdVersionForCgroupFreeze is the first envd we trust to freeze user
// cgroups across pause/resume. /freeze plus /init thaw landed in 0.6.0, but
// 0.6.0-0.6.2 also froze the socat cgroup, breaking port forwarding (fixed
// in 0.6.3 by #2923), so we gate on 0.6.3.
const MinEnvdVersionForCgroupFreeze = "0.6.3"

// MinEnvdVersionForHeapCollapse is the first envd that exposes a native
// POST /collapse endpoint, which compacts envd's own anonymous heap into 2 MiB
// hugepages before pause to reduce the frames it faults on resume. 0.6.4
// already exists in the fleet without /collapse, so the gate must be 0.6.5 (the
// version that introduces the endpoint) to avoid POSTing /collapse at a 0.6.4
// envd that 404s.
const MinEnvdVersionForHeapCollapse = "0.6.5"

// MinEnvdVersionForFsFreeze is the first envd that exposes the native
// POST /fsfreeze and /fsthaw endpoints, which quiesce the guest rootfs before a
// filesystem-only pause. Older envds fall back to a plain guest sync.
const MinEnvdVersionForFsFreeze = "0.6.6"

// MinEnvdVersionForUpgrade is the first envd that both exposes the live-upgrade
// POST /upgrade endpoint and writes the protobuf handover blob the incoming envd
// decodes. The resume-time auto-upgrade trigger delivers to the *running* (old)
// envd, which serializes the handover, so anything older either lacks /upgrade
// or writes the pre-proto (JSON) format the new envd can't read — both must be
// skipped.
const MinEnvdVersionForUpgrade = "0.6.12"

func sanitizeVersion(version string) string {
	if len(version) > 0 && version[0] != 'v' {
		version = "v" + version
	}

	return version
}

func CheckEnvdVersionForSnapshot(envdVersion string) error {
	ok, err := IsGTEVersion(envdVersion, MinEnvdVersionForSnapshot)
	if err != nil {
		return fmt.Errorf("invalid envd version %q: %w", envdVersion, err)
	}

	if !ok {
		return fmt.Errorf("sandbox envd version must be at least %s to create snapshots, current version: %s", MinEnvdVersionForSnapshot, envdVersion)
	}

	return nil
}

func IsGTEVersion(curVersion, minVersion string) (bool, error) {
	curVersion = sanitizeVersion(curVersion)
	minVersion = sanitizeVersion(minVersion)

	if !semver.IsValid(curVersion) {
		return false, fmt.Errorf("invalid current version format: %s", curVersion)
	}

	if !semver.IsValid(minVersion) {
		return false, fmt.Errorf("invalid minimum version format: %s", minVersion)
	}

	return semver.Compare(curVersion, minVersion) >= 0, nil
}
```

---

## 7. Sizing constraints

### 7.1 vCPU and memory (enforced only in the API; the orchestrator does not validate them)

vCPU: default 2, min 1, max 32, must be 1 or even, and <= team `MaxVcpu`. Memory: default 1024 MiB,
min 128, must be even, and <= team `MaxRamMb`. (Team limits come from the DB tier; the seed tier was
`vcpu 2, ram_mb 512, disk_mb 512`.)

`packages/api/internal/constants/templates.go` lines 1-9 (of 9)

```go
package constants

const (
	MinTemplateCPU        = int64(1)
	MaxTemplateCPU        = int64(32)
	MinTemplateMemory     = int64(128)
	DefaultTemplateCPU    = int64(2)
	DefaultTemplateMemory = int64(1024)
)
```

`packages/api/internal/team/limits.go` lines 1-81 (of 81)

```go
package team

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/constants"
	"github.com/e2b-dev/infra/packages/auth/pkg/types"
)

func LimitResources(limits *types.TeamLimits, cpuCount, memoryMB *int32) (int64, int64, *api.APIError) {
	cpu := constants.DefaultTemplateCPU
	ramMB := constants.DefaultTemplateMemory

	if cpuCount != nil {
		cpu = int64(*cpuCount)
		if cpu < constants.MinTemplateCPU {
			return 0, 0, &api.APIError{
				Err:       fmt.Errorf("CPU count must be at least %d", constants.MinTemplateCPU),
				ClientMsg: fmt.Sprintf("CPU count must be at least %d", constants.MinTemplateCPU),
				Code:      http.StatusBadRequest,
			}
		}

		if cpu > constants.MaxTemplateCPU {
			return 0, 0, &api.APIError{
				Err:       fmt.Errorf("CPU count must be at most %d", constants.MaxTemplateCPU),
				ClientMsg: fmt.Sprintf("CPU count must be at most %d", constants.MaxTemplateCPU),
				Code:      http.StatusBadRequest,
			}
		}

		if cpu != 1 && cpu%2 != 0 {
			return 0, 0, &api.APIError{
				Err:       errors.New("CPU count must be 1 or an even number"),
				ClientMsg: "CPU count must be 1 or an even number",
				Code:      http.StatusBadRequest,
			}
		}

		if cpu > limits.MaxVcpu {
			return 0, 0, &api.APIError{
				Err:       fmt.Errorf("CPU count exceeds team limits (%d)", limits.MaxVcpu),
				ClientMsg: fmt.Sprintf("CPU count can't be higher than %d (if you need to increase this limit, please contact support)", limits.MaxVcpu),
				Code:      http.StatusBadRequest,
			}
		}
	}

	if memoryMB != nil {
		ramMB = int64(*memoryMB)

		if ramMB < constants.MinTemplateMemory {
			return 0, 0, &api.APIError{
				Err:       fmt.Errorf("memory must be at least %d MiB", constants.MinTemplateMemory),
				ClientMsg: fmt.Sprintf("Memory must be at least %d MiB", constants.MinTemplateMemory),
				Code:      http.StatusBadRequest,
			}
		}

		if ramMB%2 != 0 {
			return 0, 0, &api.APIError{
				Err:       errors.New("user provided memory size isn't divisible by 2"),
				ClientMsg: "Memory must be divisible by 2",
				Code:      http.StatusBadRequest,
			}
		}

		if ramMB > limits.MaxRamMb {
			return 0, 0, &api.APIError{
				Err:       fmt.Errorf("memory exceeds team limits (%d MiB)", limits.MaxRamMb),
				ClientMsg: fmt.Sprintf("Memory can't be higher than %d MiB (if you need to increase this limit, please contact support)", limits.MaxRamMb),
				Code:      http.StatusBadRequest,
			}
		}
	}

	return cpu, ramMB, nil
}
```

`packages/api/internal/team/free_disk.go` lines 1-43 (of 43)

```go
package team

import (
	"fmt"
	"net/http"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/auth/pkg/types"
)

// LimitFreeDiskSize resolves the free-space growth target a build asked for
// against the team's allowance.
//
// A nil request is the team's default target. An explicit 0 disables requested
// growth and is kept as 0, so nothing here may treat the value as a flag.
func LimitFreeDiskSize(limits *types.TeamLimits, requestedMB *int32) (int64, *api.APIError) {
	if requestedMB == nil {
		return limits.DefaultFreeDiskSizeMb, nil
	}

	requested := int64(*requestedMB)

	if requested < 0 {
		return 0, &api.APIError{
			Err:       fmt.Errorf("minimum free disk must not be negative, got %d MiB", requested),
			ClientMsg: "Minimum free disk can't be negative",
			Code:      http.StatusBadRequest,
		}
	}

	if requested > limits.MaxFreeDiskSizeMb {
		return 0, &api.APIError{
			Err: fmt.Errorf("minimum free disk %d MiB exceeds team limits (%d MiB)",
				requested, limits.MaxFreeDiskSizeMb),
			ClientMsg: fmt.Sprintf(
				"Minimum free disk can't be higher than %d MiB (if you need to increase this limit, please contact support)",
				limits.MaxFreeDiskSizeMb),
			Code: http.StatusBadRequest,
		}
	}

	return requested, nil
}
```

### 7.2 Disk: `diskSizeMB`, `freeDiskSizeMB`, `total_disk_size_mb`

(Commentary) `diskSizeMB` is the free working space ensured in the base rootfs before build steps
(`Enlarge` by `diskSizeMB - currentFree`). `freeDiskSizeMB` is the free-space target the
`ensurefreedisk` phase grows the rootfs to after the steps (shortage + 10%, rounded up to 1 MiB).
The final rootfs size (MB) is returned as `TemplateBuildMetadata.rootfsSizeKey`; the API stores it as
`total_disk_size_mb` and sends it in `SandboxConfig.total_disk_size_mb`, which the orchestrator uses
only for metrics/accounting.

`packages/orchestrator/pkg/template/build/core/rootfs/rootfs.go` lines 155-195 (of 282)

```go

	l.Info(ctx, "Creating file system and pulling Docker image")
	maxRootfsSize := units.MBToBytes(int64(r.featureFlags.IntFlag(ctx, featureflags.BuildBaseRootfsSizeLimitMB)))
	ext4Size, err := oci.ToExt4(ctx, l, img, rootfsPath, maxRootfsSize, template.RootfsBlockSize())
	if err != nil {
		if imgErr, ok := errors.AsType[*oci.ImageTooLargeError](err); ok {
			return containerregistry.Config{}, phases.NewPhaseBuildError(phaseMetadata, imgErr)
		}

		return containerregistry.Config{}, fmt.Errorf("error converting oci to ext4: %w", err)
	}
	telemetry.ReportEvent(childCtx, "created rootfs ext4 file")

	l.Debug(ctx, "Filesystem cleanup")
	// Make rootfs writable, be default it's readonly
	err = filesystem.MakeWritable(ctx, rootfsPath)
	if err != nil {
		return containerregistry.Config{}, fmt.Errorf("error making rootfs file writable: %w", err)
	}

	// Resize rootfs
	rootfsFreeSpace, err := filesystem.GetFreeSpace(ctx, rootfsPath, template.RootfsBlockSize())
	if err != nil {
		return containerregistry.Config{}, fmt.Errorf("error getting free space: %w", err)
	}
	// We need to remove the remaining free space from the ext4 file size
	// This is a residual space that could not be shrunk when creating the filesystem,
	// but is still available for use
	diskAdd := units.MBToBytes(template.DiskSizeMB) - rootfsFreeSpace
	logger.L().Debug(ctx, "adding disk size diff to rootfs",
		zap.Int64("size_current", ext4Size),
		zap.Int64("size_add", diskAdd),
		zap.Int64("size_free", rootfsFreeSpace),
	)
	if diskAdd > 0 {
		_, err := filesystem.Enlarge(ctx, rootfsPath, diskAdd)
		if err != nil {
			return containerregistry.Config{}, fmt.Errorf("error enlarging rootfs: %w", err)
		}
	}

```

`packages/orchestrator/pkg/template/build/phases/ensurefreedisk/sizing.go` lines 1-55 (of 75)

```go
package ensurefreedisk

import (
	"errors"
	"fmt"
	"math"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/units"
)

// computeGrownSize assumes the caller has already validated the source
// geometry (see validateSourceGeometry), including that the block size
// matches header.RootfsBlockSize.
func computeGrownSize(currentSize, targetFree, freeBefore int64) (int64, error) {
	if currentSize <= 0 {
		return 0, fmt.Errorf("invalid current size: %d", currentSize)
	}
	if targetFree < 0 {
		return 0, fmt.Errorf("invalid target free space: %d", targetFree)
	}
	if freeBefore >= targetFree {
		return 0, fmt.Errorf("free space before (%d) is already at or above target (%d)", freeBefore, targetFree)
	}
	if freeBefore < 0 && targetFree > math.MaxInt64+freeBefore {
		return 0, errors.New("free-space shortage overflows")
	}

	shortage := targetFree - freeBefore
	// resize2fs uses some new capacity for ext4 metadata, so allow 10% on the
	// measured shortage. The post-resize free-space measurement is authoritative.
	extraGrowth := shortage / 10
	if shortage%10 != 0 {
		extraGrowth++
	}
	if shortage > math.MaxInt64-extraGrowth {
		return 0, errors.New("free-space growth overflows")
	}
	growth := shortage + extraGrowth
	if currentSize > math.MaxInt64-growth {
		return 0, errors.New("grown size overflows")
	}

	size := currentSize + growth
	resizeUnit := units.MBToBytes(1)
	if remainder := size % resizeUnit; remainder != 0 {
		add := resizeUnit - remainder
		if size > math.MaxInt64-add {
			return 0, errors.New("aligned grown size overflows")
		}
		size += add
	}

	return size, nil
}
```

`packages/orchestrator/pkg/sandbox/sandbox.go` lines 120-135 (of 4288)

```go
type Config struct {
	// TODO: Remove when the rootfs path is constant.
	// Only used for v1 rootfs paths format.
	BaseTemplateID string

	Vcpu  int64
	RamMB int64

	// TotalDiskSizeMB optional, now used only for metrics.
	TotalDiskSizeMB   int64
	HugePages         bool
	FreePageReporting bool
	FreePageHinting   bool

	Envd EnvdMetadata

```

### 7.3 Hugepages

(Commentary) `huge_pages` must equal what the template was built with (the memfile page size is 2 MiB
with hugepages, 4 KiB without), and is true for every FC >= 1.7, i.e. all current defaults. With it,
Firecracker is configured with `HugePages: 2M`, so guest RAM comes from the host's 2 MiB HugeTLB pool:
the host needs `vm.nr_hugepages` large enough for the sum of running sandboxes' `ram_mb`/2 pages
(InfoService reports `metric_hugepages_*`). Memory must be even (API check), which keeps it a multiple of 2 MiB.

`packages/orchestrator/pkg/sandbox/fc/client.go` lines 378-410 (of 699)

```go
	ctx context.Context,
	vCPUCount int64,
	memoryMB int64,
	hugePages bool,
) error {
	// SMT (Simultaneous Multi-Threading / Hyper-Threading) must be disabled on
	// ARM64 because ARM processors use a different core topology (big.LITTLE,
	// efficiency/performance cores) rather than hardware threads per core.
	// Firecracker validates this against the host CPU and rejects SMT=true on ARM.
	// See: https://github.com/firecracker-microvm/firecracker/blob/main/docs/cpu_templates/cpu-features.md
	// We use runtime.GOARCH (not TARGET_ARCH) because the orchestrator binary
	// always runs on the same architecture as Firecracker.
	smt := runtime.GOARCH != archARM64
	trackDirtyPages := false
	machineConfig := &models.MachineConfiguration{
		VcpuCount:       &vCPUCount,
		MemSizeMib:      &memoryMB,
		Smt:             &smt,
		TrackDirtyPages: &trackDirtyPages,
	}
	if hugePages {
		machineConfig.HugePages = models.MachineConfigurationHugePagesNr2M
	}
	machineConfigParams := operations.PutMachineConfigurationParams{
		Context: ctx,
		Body:    machineConfig,
	}
	_, err := c.client.Operations.PutMachineConfiguration(&machineConfigParams)
	if err != nil {
		return fmt.Errorf("error setting fc machine config: %w", err)
	}

	return nil
```

`packages/orchestrator/pkg/sandbox/sandbox.go` lines 983-992 (of 4288)

```go

	fcPageSize := int64(header.PageSize)
	if config.HugePages {
		fcPageSize = int64(header.HugepageSize)
	}
	resources := &Resources{
		Slot:   ips,
		rootfs: rootfsProvider,
		memory: uffd.NewNoopMemory(memfileSize, fcPageSize),
	}
```

`packages/orchestrator/pkg/template/build/config/config.go` lines 103-109 (of 113)

```go
func MemfilePageSize(hugePages bool) int64 {
	if hugePages {
		return header.HugepageSize
	}

	return header.PageSize
}
```

`DEV-LOCAL.md` lines 28-40 (of 264)

````markdown
   sudo modprobe nbd nbds_max=64
   ```
   Verify: `lsmod | grep nbd` should show the module loaded.

2. Enable huge pages:
   ```bash
   sudo sysctl -w vm.nr_hugepages=2048
   ```
   Verify: `grep HugePages_Total /proc/meminfo` should show `2048`.

> To persist these across reboots, add `nbd nbds_max=64` to `/etc/modules-load.d/nbd.conf` and `vm.nr_hugepages=2048` to `/etc/sysctl.d/99-hugepages.conf`.

## Download prebuilt artifacts (customized firecrackers and linux kernels)
````

`packages/orchestrator/README.md` lines 16-24 (of 323)

````markdown
```

**Prerequisite:** Allocate enough 2MB HugeTLB pages (hugepages) for testing:

```bash
echo 1024 | sudo tee /proc/sys/vm/nr_hugepages   # for 2GB hugepages, adjust as needed
```

### Create Build
````

---

## 8. IDs: formats and validation

- `sandbox_id`: `"i" + id.Generate()` = `i` + 20 chars of `[a-z0-9]`. Proxy and API validate `^[a-z0-9]+$` (no dashes, because the Host format uses `-` as separator). The orchestrator reserves it per node (must be unique among running sandboxes).
- `template_id`: `id.Generate()` (20 chars `[a-z0-9]`); aliases/namespaces use `^[a-z0-9-_]+$`, tags `^[a-z0-9-_.]+$`. Snapshot template IDs also come from `id.Generate()`.
- `build_id`: UUID (`uuid.NewRandom()` in the API; DB default `gen_random_uuid()`); orchestrator `uuid.Parse`s it when pausing, and diff headers are keyed by build UUID.
- `execution_id`: `uuid.New().String()`, fresh on every start/resume/fork.
- `team_id`: UUID string (`team.ID.String()`); orchestrator `uuid.Parse`s it for events/host stats and logs an error (falls back to zero UUID) if not a UUID.
- `SandboxCreateResponse.client_id`: legacy, the node's client ID; the API uses the constant `6532622b`.

`packages/shared/pkg/id/id.go` lines 1-164 (of 164)

```go
package id

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/dchest/uniuri"
	"github.com/google/uuid"
)

var (
	caseInsensitiveAlphabet = []byte("abcdefghijklmnopqrstuvwxyz1234567890")
	identifierRegex         = regexp.MustCompile(`^[a-z0-9-_]+$`)
	tagRegex                = regexp.MustCompile(`^[a-z0-9-_.]+$`)
	sandboxIDRegex          = regexp.MustCompile(`^[a-z0-9]+$`)
)

const (
	DefaultTag         = "default"
	TagSeparator       = ":"
	NamespaceSeparator = "/"
)

func Generate() string {
	return uniuri.NewLenChars(uniuri.UUIDLen, caseInsensitiveAlphabet)
}

// ValidateSandboxID checks that a sandbox ID contains only lowercase alphanumeric characters.
func ValidateSandboxID(sandboxID string) error {
	if !sandboxIDRegex.MatchString(sandboxID) {
		return fmt.Errorf("invalid sandbox ID: %q", sandboxID)
	}

	return nil
}

func cleanAndValidate(value, name string, re *regexp.Regexp) (string, error) {
	cleaned := strings.ToLower(strings.TrimSpace(value))
	if !re.MatchString(cleaned) {
		return "", fmt.Errorf("invalid %s: %s", name, value)
	}

	return cleaned, nil
}

func validateTag(tag string) (string, error) {
	cleanedTag, err := cleanAndValidate(tag, "tag", tagRegex)
	if err != nil {
		return "", err
	}

	// Prevent tags from being a UUID
	_, err = uuid.Parse(cleanedTag)
	if err == nil {
		return "", errors.New("tag cannot be a UUID")
	}

	return cleanedTag, nil
}

func ValidateAndDeduplicateTags(tags []string) ([]string, error) {
	seen := make(map[string]struct{})

	for _, tag := range tags {
		cleanedTag, err := validateTag(tag)
		if err != nil {
			return nil, fmt.Errorf("invalid tag '%s': %w", tag, err)
		}

		seen[cleanedTag] = struct{}{}
	}

	return slices.Collect(maps.Keys(seen)), nil
}

// SplitIdentifier splits "namespace/alias" into its parts.
// Returns nil namespace for bare aliases, pointer for explicit namespace.
func SplitIdentifier(identifier string) (namespace *string, alias string) {
	before, after, found := strings.Cut(identifier, NamespaceSeparator)
	if !found {
		return nil, before
	}

	return &before, after
}

// ParseName parses and validates "namespace/alias:tag" or "alias:tag".
// Returns the cleaned identifier (namespace/alias or alias) and optional tag.
// All components are validated and normalized (lowercase, trimmed).
func ParseName(input string) (identifier string, tag *string, err error) {
	input = strings.TrimSpace(input)

	// Extract raw parts
	identifierPart, tagPart, hasTag := strings.Cut(input, TagSeparator)
	namespacePart, aliasPart := SplitIdentifier(identifierPart)

	// Validate tag
	if hasTag {
		validated, err := cleanAndValidate(tagPart, "tag", tagRegex)
		if err != nil {
			return "", nil, err
		}
		if !strings.EqualFold(validated, DefaultTag) {
			tag = &validated
		}
	}

	// Validate namespace
	if namespacePart != nil {
		validated, err := cleanAndValidate(*namespacePart, "namespace", identifierRegex)
		if err != nil {
			return "", nil, err
		}
		namespacePart = &validated
	}

	// Validate alias
	aliasPart, err = cleanAndValidate(aliasPart, "template ID", identifierRegex)
	if err != nil {
		return "", nil, err
	}

	// Build identifier
	if namespacePart != nil {
		identifier = WithNamespace(*namespacePart, aliasPart)
	} else {
		identifier = aliasPart
	}

	return identifier, tag, nil
}

// WithTag returns the identifier with the given tag appended (e.g. "templateID:tag").
func WithTag(identifier, tag string) string {
	return identifier + TagSeparator + tag
}

// WithNamespace returns identifier with the given namespace prefix.
func WithNamespace(namespace, alias string) string {
	return namespace + NamespaceSeparator + alias
}

// ExtractAlias returns just the alias portion from an identifier (namespace/alias or alias).
func ExtractAlias(identifier string) string {
	_, alias := SplitIdentifier(identifier)

	return alias
}

// ValidateNamespaceMatchesTeam checks if an explicit namespace in the identifier matches the team's slug.
// Returns an error if the namespace doesn't match.
// If the identifier has no explicit namespace, returns nil (valid).
func ValidateNamespaceMatchesTeam(identifier, teamSlug string) error {
	namespace, _ := SplitIdentifier(identifier)
	if namespace != nil && *namespace != teamSlug {
		return fmt.Errorf("namespace '%s' must match your team '%s'", *namespace, teamSlug)
	}

	return nil
}
```

`github.com/dchest/uniuri@v1.2.0/uniuri.go` lines 30-40

```go
const (
	// StdLen is a standard length of uniuri string to achive ~95 bits of entropy.
	StdLen = 16
	// UUIDLen is a length of uniuri string to achive ~119 bits of entropy, closest
	// to what can be losslessly converted to UUIDv4 (122 bits).
	UUIDLen = 20
)

// StdChars is a set of standard characters allowed in uniuri string.
var StdChars = []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789")

```

`packages/api/internal/utils/split.go` lines 1-26 (of 26)

```go
package utils

import (
	"fmt"
	"strings"

	"github.com/e2b-dev/infra/packages/shared/pkg/id"
)

func ShortID(compositeID string) (string, error) {
	parts := strings.Split(compositeID, "-")
	if len(parts) > 2 {
		return "", fmt.Errorf("invalid sandbox ID: %q", compositeID)
	}

	sandboxID := compositeID
	if len(parts) == 2 {
		sandboxID = parts[0]
	}

	if err := id.ValidateSandboxID(sandboxID); err != nil {
		return "", err
	}

	return sandboxID, nil
}
```

`packages/api/internal/handlers/sandbox_create.go` lines 172-176 (of 1038)

```go
	setTemplateNameMetric(ctx, c, a.featureFlags, env.TemplateID, env.Names)

	sandboxID := InstanceIDPrefix + id.Generate()

	c.Set("instanceID", sandboxID)
```

`packages/api/internal/template/register_build.go` lines 149-159 (of 489)

```go
	// Generate a build id for the new build
	buildID, err := uuid.NewRandom()
	if err != nil {
		telemetry.ReportCriticalError(ctx, "error when generating build id", err)

		return nil, &api.APIError{
			Err:       err,
			ClientMsg: "Failed to generate build id",
			Code:      http.StatusInternalServerError,
		}
	}
```

`packages/db/migrations/20240315165236_create_env_builds.sql` lines 9-9 (of 31)

```sql
CREATE TABLE "public"."env_builds" ("id" uuid NOT NULL DEFAULT gen_random_uuid(), "created_at" timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP, "updated_at" timestamptz NOT NULL, "finished_at" timestamptz NULL, "status" text NOT NULL DEFAULT 'waiting', "dockerfile" text NULL, "start_cmd" text NULL, "vcpu" bigint NOT NULL, "ram_mb" bigint NOT NULL, "free_disk_size_mb" bigint NOT NULL, "total_disk_size_mb" bigint NULL, "kernel_version" text NOT NULL DEFAULT 'vmlinux-5.10.186', "firecracker_version" text NOT NULL DEFAULT 'v1.7.0-dev_8bb88311', "env_id" text NULL, PRIMARY KEY ("id"), CONSTRAINT "env_builds_envs_builds" FOREIGN KEY ("env_id") REFERENCES "public"."envs" ("id") ON UPDATE NO ACTION ON DELETE CASCADE);
```

`packages/orchestrator/pkg/server/sandboxes.go` lines 1627-1633 (of 2570)

```go

// Extracts common data needed for sandbox events
func (s *Server) prepareSandboxEventData(ctx context.Context, sbx *sandbox.Sandbox) (uuid.UUID, string, int64, map[string]any) {
	teamID, err := uuid.Parse(sbx.Runtime.TeamID)
	if err != nil {
		sbxlogger.I(sbx).Error(ctx, "error parsing team ID", logger.WithSandboxID(sbx.Runtime.SandboxID), zap.Error(err))
	}
```

`packages/orchestrator/pkg/sandbox/hoststats.go` lines 18-30 (of 49)

```go
func initializeHostStatsCollector(
	ctx context.Context,
	sbx *Sandbox,
	runtime sandboxtypes.RuntimeMetadata,
	config *Config,
	hostStatsDelivery hoststats.Delivery,
) {
	teamID, err := uuid.Parse(runtime.TeamID)
	if err != nil {
		logger.L().Warn(ctx, "invalid team ID for host stats, using zero UUID",
			logger.WithTeamID(runtime.TeamID), zap.Error(err))
	}

```

---

## Appendix: files quoted from outside the repo

- `ext/ggcr-v0.21.7-pkg-name-registry.go`: `git show v0.21.7:pkg/name/registry.go` from github.com/google/go-containerregistry (version pinned in packages/orchestrator/go.mod line 43).
- `ext/uniuri-v1.2.0-uniuri.go`: github.com/dchest/uniuri v1.2.0 (pinned in packages/shared/go.mod line 25).
- connect-go snippet: `~/go/pkg/mod/connectrpc.com/connect@v1.20.0/duplex_http_call.go` lines 295-306 (repo pins v1.18.1).
