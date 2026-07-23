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
type Client struct {
	conn   *grpc.ClientConn
	client proto.HyperClient
}

// NewClient dials the Hyper cluster at addr.
func NewClient(addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial hyper %s: %w", addr, err)
	}
	return &Client{
		conn:   conn,
		client: proto.NewHyperClient(conn),
	}, nil
}

// CreateVm creates and boots a microVM from an image.
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

// StopVm stops and tears down a microVM.
func (c *Client) StopVm(ctx context.Context, vmID string) error {
	_, err := c.client.StopVm(ctx, &proto.StopVmRequest{VmId: vmID})
	return err
}

// GetVm locates a microVM and reports its node.
func (c *Client) GetVm(ctx context.Context, vmID string) (*proto.GetVmResponse, error) {
	return c.client.GetVm(ctx, &proto.GetVmRequest{VmId: vmID})
}

// ListVms lists all microVMs in the cluster.
func (c *Client) ListVms(ctx context.Context) (*proto.ListVmsResponse, error) {
	return c.client.ListVms(ctx, &emptypb.Empty{})
}

// Close releases the gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}
