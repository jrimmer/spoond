// Package e2b implements substrate.Substrate against E2B's orchestrator
// (gRPC on 127.0.0.1:5008) and envd (Connect-RPC through the orchestrator
// proxy on 127.0.0.1:5007).
package e2b

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jrimmer/spoond/v2/substrate"
	// gen/info declares "package orchestrator" and gen/template declares
	// "package template_manager" (the fork's go_package URLs win over the M
	// flags' import paths, per protoc-gen-go), so import them under the
	// directory names.
	info "github.com/jrimmer/spoond/v2/substrate/e2b/gen/info"
	orchestrator "github.com/jrimmer/spoond/v2/substrate/e2b/gen/orchestrator"
	template "github.com/jrimmer/spoond/v2/substrate/e2b/gen/template"
)

// maxSandboxLength is the sandbox length limit E2B's API sends on every
// create, in hours.
const maxSandboxLength = 720

// Client implements substrate.Substrate against the E2B orchestrator.
type Client struct {
	cfg Config

	conn     *grpc.ClientConn
	sandbox  orchestrator.SandboxServiceClient
	template template.TemplateServiceClient
	info     info.InfoServiceClient

	http *http.Client
}

// New dials the orchestrator (plaintext gRPC) and returns a Client.
func New(cfg Config) (*Client, error) {
	conn, err := grpc.NewClient(cfg.GRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("e2b: dial %s: %w", cfg.GRPCAddr, err)
	}
	return &Client{
		cfg:      cfg,
		conn:     conn,
		sandbox:  orchestrator.NewSandboxServiceClient(conn),
		template: template.NewTemplateServiceClient(conn),
		info:     info.NewInfoServiceClient(conn),
		http:     &http.Client{},
	}, nil
}

// mapError converts orchestrator gRPC errors to substrate sentinel errors,
// wrapped with the gRPC message.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	switch status.Code(err) {
	case codes.NotFound:
		return fmt.Errorf("%w: %s", substrate.ErrNotFound, status.Convert(err).Message())
	case codes.ResourceExhausted:
		// The node refuses creates when the sandbox cap or the starting
		// limit is hit, and pauses/checkpoints while it persists another
		// snapshot (A2 §3.6).
		return fmt.Errorf("%w: %s", substrate.ErrCapacity, status.Convert(err).Message())
	default:
		return err
	}
}

// Create starts a sandbox and waits for envd to become healthy.
func (c *Client) Create(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
	id := req.SandboxID
	executionID := NewUUID() // new on every Create
	start := timestamppb.Now()
	config := &orchestrator.SandboxConfig{
		TemplateId:         req.TemplateID,
		BaseTemplateId:     req.TemplateID,
		TeamId:             c.cfg.TeamID,
		BuildId:            req.BuildID,
		SandboxId:          id,
		ExecutionId:        executionID,
		KernelVersion:      req.KernelVersion,
		FirecrackerVersion: req.FirecrackerVersion,
		EnvdVersion:        req.EnvdVersion,
		EnvVars:            req.EnvVars,
		Metadata:           req.Metadata,
		EnvdAccessToken:    proto.String(c.EnvdToken(id)),
		MaxSandboxLength:   maxSandboxLength,
		HugePages:          true,
		RamMb:              int64(req.MemoryMB),
		Vcpu:               int64(req.VCPU),
		TotalDiskSizeMb:    int64(req.DiskSizeMB),
		Snapshot:           req.Resume,
		Network: &orchestrator.SandboxNetworkConfig{
			Egress:  egressConfig(req.Egress),
			Ingress: &orchestrator.SandboxNetworkIngressConfig{TrafficAccessToken: proto.String(c.TrafficToken(id))},
		},
	}
	resp, err := c.sandbox.Create(ctx, &orchestrator.SandboxCreateRequest{
		Sandbox:   config,
		StartTime: start,
		EndTime:   timestamppb.New(req.EndAt),
	})
	if err != nil {
		return substrate.Sandbox{}, mapError(err)
	}

	// Poll envd health until the sandbox answers, then hand it out.
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for {
		lastErr = c.Health(ctx, id)
		if lastErr == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = c.Delete(context.WithoutCancel(ctx), id)
			return substrate.Sandbox{}, fmt.Errorf("e2b: create %s: envd not healthy within 30s: %w", id, lastErr)
		}
		select {
		case <-ctx.Done():
			_ = c.Delete(context.WithoutCancel(ctx), id)
			return substrate.Sandbox{}, fmt.Errorf("e2b: create %s: %w", id, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}

	return substrate.Sandbox{
		ID:          id,
		ExecutionID: executionID,
		TemplateID:  req.TemplateID,
		BuildID:     req.BuildID,
		HostIP:      resp.GetHostIp(),
		VCPU:        req.VCPU,
		MemoryMB:    req.MemoryMB,
		StartedAt:   start.AsTime(),
		EndAt:       req.EndAt,
	}, nil
}

// List returns every running sandbox.
func (c *Client) List(ctx context.Context) ([]substrate.Sandbox, error) {
	resp, err := c.sandbox.List(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]substrate.Sandbox, 0, len(resp.GetSandboxes()))
	for _, rs := range resp.GetSandboxes() {
		out = append(out, substrate.Sandbox{
			ID:          rs.GetSandboxId(),
			ExecutionID: rs.GetExecutionId(),
			TemplateID:  rs.GetConfig().GetTemplateId(),
			BuildID:     rs.GetConfig().GetBuildId(),
			HostIP:      rs.GetHostIp(),
			VCPU:        uint32(rs.GetVcpu()),
			MemoryMB:    uint32(rs.GetRamMb()),
			StartedAt:   rs.GetStartTime().AsTime(),
			EndAt:       rs.GetEndTime().AsTime(),
		})
	}
	return out, nil
}

// Delete stops a sandbox; nil when already gone.
func (c *Client) Delete(ctx context.Context, sandboxID string) error {
	_, err := c.sandbox.Delete(ctx, &orchestrator.SandboxDeleteRequest{SandboxId: sandboxID})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil
		}
		return mapError(err)
	}
	return nil
}

// Pause snapshots a sandbox to a new build and stops it.
func (c *Client) Pause(ctx context.Context, sandboxID, templateID string) (string, substrate.BuildRefs, error) {
	before := c.outstandingLevel(ctx)
	buildID := NewUUID()
	resp, err := c.sandbox.Pause(ctx, &orchestrator.SandboxPauseRequest{
		SandboxId:  sandboxID,
		TemplateId: templateID,
		BuildId:    buildID,
	})
	if err != nil {
		return "", substrate.BuildRefs{}, mapError(err)
	}
	c.waitOutstanding(ctx, sandboxID, "pause", before)
	return buildID, refsFrom(resp.GetSchedulingMetadata()), nil
}

// Checkpoint snapshots a running sandbox to a new build; it keeps running.
func (c *Client) Checkpoint(ctx context.Context, sandboxID string) (string, substrate.BuildRefs, error) {
	before := c.outstandingLevel(ctx)
	buildID := NewUUID()
	resp, err := c.sandbox.Checkpoint(ctx, &orchestrator.SandboxCheckpointRequest{
		SandboxId: sandboxID,
		BuildId:   buildID,
	})
	if err != nil {
		return "", substrate.BuildRefs{}, mapError(err)
	}
	c.waitOutstanding(ctx, sandboxID, "checkpoint", before)
	return buildID, refsFrom(resp.GetSchedulingMetadata()), nil
}

const (
	outstandingPollInterval = 500 * time.Millisecond
	outstandingWaitBound    = 10 * time.Second
)

// outstandingLevel captures the node's outstanding work just before a pause
// or checkpoint call, so the wait afterwards can detect when the node is back
// at (or under) its pre-call level.
func (c *Client) outstandingLevel(ctx context.Context) int {
	if info, err := c.NodeInfo(ctx); err == nil {
		return info.OutstandingWork
	}
	return 0
}

func (c *Client) waitOutstanding(ctx context.Context, sandboxID, method string, before int) {
	waitOutstanding(ctx, sandboxID, method, before, c.NodeInfo, outstandingPollInterval, outstandingWaitBound)
}

// waitOutstanding polls NodeInfo until the node's outstanding work drops back
// to before (the level captured at the gRPC call) or the bound elapses. The
// Pause/Checkpoint gRPC response already confirms the snapshot is durable;
// this wait only yields to persist work still in flight on the node
// (outstanding_work counts every tracked operation, A3 A6, and does not
// always return to 0), so it is bounded and never an error: on timeout we log
// and continue.
func waitOutstanding(ctx context.Context, sandboxID, method string, before int, nodeInfo func(context.Context) (substrate.NodeInfo, error), poll, bound time.Duration) {
	last := before
	deadline := time.Now().Add(bound)
	for {
		if info, err := nodeInfo(ctx); err == nil {
			last = info.OutstandingWork
			if last <= before {
				return
			}
		}
		if time.Now().After(deadline) {
			slog.Warn(fmt.Sprintf("e2b: %s %s: outstanding_work still %d after %s", method, sandboxID, last, bound))
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}
}

// UpdateEgress replaces a sandbox's egress policy.
func (c *Client) UpdateEgress(ctx context.Context, sandboxID string, eg substrate.Egress) error {
	_, err := c.sandbox.Update(ctx, &orchestrator.SandboxUpdateRequest{
		SandboxId: sandboxID,
		Egress:    egressConfig(eg),
	})
	return mapError(err)
}

// UpdateEndAt extends or shortens a sandbox's lease expiry.
func (c *Client) UpdateEndAt(ctx context.Context, sandboxID string, endAt time.Time) error {
	_, err := c.sandbox.Update(ctx, &orchestrator.SandboxUpdateRequest{
		SandboxId: sandboxID,
		EndTime:   timestamppb.New(endAt),
	})
	return mapError(err)
}

// NodeInfo returns the orchestrator node's status and metrics.
func (c *Client) NodeInfo(ctx context.Context) (substrate.NodeInfo, error) {
	resp, err := c.info.ServiceInfo(ctx, &emptypb.Empty{})
	if err != nil {
		return substrate.NodeInfo{}, mapError(err)
	}
	return substrate.NodeInfo{
		Status:             statusString(resp.GetServiceStatus()),
		Version:            resp.GetServiceVersion(),
		RunningSandboxes:   int(resp.GetMetricSandboxesRunning()),
		OutstandingWork:    int(resp.GetOutstandingWork()),
		HugepagesTotal:     resp.GetMetricHugepagesTotal(),
		HugepagesUsed:      resp.GetMetricHugepagesUsed(),
		HugepagesReserved:  resp.GetMetricHugepagesReserved(),
		HugepageSizeBytes:  resp.GetMetricHugepageSizeBytes(),
		EnvdVersion:        c.cfg.EnvdVersion,
		FirecrackerVersion: c.cfg.FirecrackerVersion,
	}, nil
}

func statusString(s info.ServiceInfoStatus) string {
	switch s {
	case info.ServiceInfoStatus_Healthy:
		return "healthy"
	case info.ServiceInfoStatus_Draining:
		return "draining"
	case info.ServiceInfoStatus_Unhealthy:
		return "unhealthy"
	case info.ServiceInfoStatus_Standby:
		return "standby"
	case info.ServiceInfoStatus_ShuttingDown:
		return "shutting_down"
	default:
		return "unknown"
	}
}

// SetDraining puts the node into (or out of) the draining state.
func (c *Client) SetDraining(ctx context.Context, draining bool) error {
	s := info.ServiceInfoStatus_Healthy
	if draining {
		s = info.ServiceInfoStatus_Draining
	}
	_, err := c.info.ServiceStatusOverride(ctx, &info.ServiceStatusChangeRequest{ServiceStatus: s})
	return mapError(err)
}

// BuildTemplate starts a template build and polls it to completion.
func (c *Client) BuildTemplate(ctx context.Context, req substrate.BuildRequest) (substrate.BuildResult, error) {
	_, err := c.template.TemplateCreate(ctx, &template.TemplateCreateRequest{
		Template: &template.TemplateConfig{
			TemplateID:   req.TemplateID,
			BuildID:      req.BuildID,
			MemoryMB:     int32(req.MemoryMB),
			VCpuCount:    int32(req.VCPU),
			DiskSizeMB:   int32(req.DiskSizeMB),
			StartCommand: req.StartCmd,
			ReadyCommand: req.ReadyCmd,
			TeamID:       c.cfg.TeamID,
			Force:        proto.Bool(false),
			Source:       &template.TemplateConfig_FromImage{FromImage: req.FromImage},
		},
	})
	if err != nil {
		return substrate.BuildResult{}, mapError(err)
	}
	// The build status cache lives 10 minutes, so poll every 2 s; no overall
	// timeout except ctx.
	var logs []string
	offset := int32(0)
	for {
		resp, err := c.template.TemplateBuildStatus(ctx, &template.TemplateStatusRequest{
			TemplateID: req.TemplateID,
			BuildID:    req.BuildID,
			Offset:     proto.Int32(offset),
		})
		if err != nil {
			return substrate.BuildResult{}, mapError(err)
		}
		for _, entry := range resp.GetLogEntries() {
			logs = append(logs, entry.GetMessage())
		}
		offset += int32(len(resp.GetLogEntries()))
		switch resp.GetStatus() {
		case template.TemplateBuildState_Completed:
			md := resp.GetMetadata()
			return substrate.BuildResult{
				BuildID:            req.BuildID,
				KernelVersion:      md.GetKernelVersion(),
				FirecrackerVersion: md.GetFirecrackerVersion(),
				EnvdVersion:        md.GetEnvdVersionKey(),
				DiskSizeMB:         uint32(md.GetRootfsSizeKey()),
				Refs:               refsFrom(md.GetSchedulingMetadata()),
				Log:                logs,
			}, nil
		case template.TemplateBuildState_Failed:
			return substrate.BuildResult{}, fmt.Errorf(
				"e2b: template build %s/%s failed: %s; last logs:\n%s",
				req.TemplateID, req.BuildID, resp.GetReason().GetMessage(), lastLines(logs, 50))
		}
		select {
		case <-ctx.Done():
			return substrate.BuildResult{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// DeleteBuild deletes the files of a template build.
func (c *Client) DeleteBuild(ctx context.Context, templateID, buildID string) error {
	_, err := c.template.TemplateBuildDelete(ctx, &template.TemplateBuildDeleteRequest{
		BuildID:    buildID,
		TemplateID: templateID,
	})
	return mapError(err)
}

// DialGuest opens a TCP connection to a guest port. This works from the host
// because the orchestrator DNATs HostIP to the guest (A3 D4); used by the SSH
// gateway for non-envd ports and by expose_ports checks.
func (c *Client) DialGuest(ctx context.Context, sandboxID, hostIP string, port int) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", hostIP+":"+strconv.Itoa(port), 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("e2b: dial guest %s %s:%d: %w", sandboxID, hostIP, port, err)
	}
	return conn, nil
}

func refsFrom(md *orchestrator.SchedulingMetadata) substrate.BuildRefs {
	if md == nil {
		return substrate.BuildRefs{}
	}
	return substrate.BuildRefs{
		RootfsBuildIDs:  md.GetRootfsBuildIds(),
		MemfileBuildIDs: md.GetMemfileBuildIds(),
	}
}

func lastLines(lines []string, n int) string {
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

var _ substrate.Substrate = (*Client)(nil)
