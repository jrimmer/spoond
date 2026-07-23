package hyper

import (
	"context"
	"fmt"

	"code.lacy.casa/jrimmer/hyper-forgejo-runner/hyper/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Client is a gRPC client for the Hyper cluster service.
// It speaks to the Hyper orchestrator at the configured address (typically
// 172.30.0.1:50051 — the Docker bridge gateway from inside a container).
type Client struct {
	conn   *grpc.ClientConn
	client proto.HyperClient
}

// NewClient dials the Hyper cluster at addr.
// The connection uses plaintext credentials; Hyper runs on the local
// network and TLS is terminated at the mesh/proxy layer.
func NewClient(addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64*1024*1024)),
	)
	if err != nil {
		return nil, fmt.Errorf("dial hyper %s: %w", addr, err)
	}
	return &Client{
		conn:   conn,
		client: proto.NewHyperClient(conn),
	}, nil
}

// CreateVm creates and boots a microVM from an image.
// imgID is a content-addressed image id (from LoadImage or a prior record).
// instanceType selects the vCPU/memory/disk size class.
// arch must match the architecture the image was built for.
func (c *Client) CreateVm(ctx context.Context, imgID string, instanceType proto.InstanceType, arch proto.Architecture) (*proto.CreateVmResponse, error) {
	resp, err := c.client.CreateVm(ctx, &proto.CreateVmRequest{
		ImgId:        imgID,
		InstanceType: instanceType,
		Arch:         arch,
	})
	if err != nil {
		return nil, fmt.Errorf("create vm: %w", err)
	}
	return resp, nil
}

// CreateVmWithBootArgs is like CreateVm but allows passing extra kernel
// command-line arguments (e.g. for debug console or custom init).
func (c *Client) CreateVmWithBootArgs(ctx context.Context, imgID string, instanceType proto.InstanceType, arch proto.Architecture, bootArgs string) (*proto.CreateVmResponse, error) {
	req := &proto.CreateVmRequest{
		ImgId:        imgID,
		InstanceType: instanceType,
		Arch:         arch,
	}
	if bootArgs != "" {
		req.BootArgs = &bootArgs
	}
	resp, err := c.client.CreateVm(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("create vm: %w", err)
	}
	return resp, nil
}

// ForkVm forks a running VM: snapshots its disk and boots a colocated child
// from the copy-on-write clone. The parent keeps running.
func (c *Client) ForkVm(ctx context.Context, vmID string) (*proto.ForkVmResponse, error) {
	resp, err := c.client.ForkVm(ctx, &proto.ForkVmRequest{VmId: vmID})
	if err != nil {
		return nil, fmt.Errorf("fork vm %s: %w", vmID, err)
	}
	return resp, nil
}

// StopVm stops and tears down a microVM. Idempotent: tearing down a VM
// that is already stopping is not an error.
func (c *Client) StopVm(ctx context.Context, vmID string) error {
	_, err := c.client.StopVm(ctx, &proto.StopVmRequest{VmId: vmID})
	if err != nil {
		return fmt.Errorf("stop vm %s: %w", vmID, err)
	}
	return nil
}

// GetVm locates a microVM and reports the cluster node it runs on.
func (c *Client) GetVm(ctx context.Context, vmID string) (*proto.GetVmResponse, error) {
	resp, err := c.client.GetVm(ctx, &proto.GetVmRequest{VmId: vmID})
	if err != nil {
		return nil, fmt.Errorf("get vm %s: %w", vmID, err)
	}
	return resp, nil
}

// GetVmUsage reports a VM's metered compute: cumulative CPU time actually
// executed, measured from its cgroup. Works for stopped VMs too.
func (c *Client) GetVmUsage(ctx context.Context, vmID string) (*proto.GetVmUsageResponse, error) {
	resp, err := c.client.GetVmUsage(ctx, &proto.GetVmUsageRequest{VmId: vmID})
	if err != nil {
		return nil, fmt.Errorf("get vm usage %s: %w", vmID, err)
	}
	return resp, nil
}

// ListVms lists all microVMs currently known to the cluster, across all nodes.
func (c *Client) ListVms(ctx context.Context) (*proto.ListVmsResponse, error) {
	resp, err := c.client.ListVms(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, fmt.Errorf("list vms: %w", err)
	}
	return resp, nil
}

// LoadImage loads an OCI image into the cluster's shared media store.
// Pulls the referenced image, flattens it, builds an ext4 rootfs, and records
// a base image. Blocks until the load completes (can take minutes).
// Returns the content-addressed img_id to pass to CreateVm.
func (c *Client) LoadImage(ctx context.Context, imageRef, label string) (*proto.LoadImageResponse, error) {
	req := &proto.LoadImageRequest{
		ImageRef: imageRef,
	}
	if label != "" {
		req.Label = &label
	}
	resp, err := c.client.LoadImage(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("load image %s: %w", imageRef, err)
	}
	return resp, nil
}

// Close releases the gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}