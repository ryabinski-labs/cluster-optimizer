package imagegc

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// CRIClient is a Runtime backed by a containerd (or any CRI) socket. It speaks
// the same gRPC API crictl uses, so it works against the unmodified runtime on
// a DOKS node without shelling out to a binary.
type CRIClient struct {
	conn    *grpc.ClientConn
	images  runtimeapi.ImageServiceClient
	runtime runtimeapi.RuntimeServiceClient
}

// DialCRI connects to the CRI socket at endpoint (e.g.
// unix:///run/containerd/containerd.sock). The caller must Close the result.
func DialCRI(ctx context.Context, endpoint string) (*CRIClient, error) {
	target := endpoint
	if !strings.Contains(target, "://") {
		target = "unix://" + target
	}
	// grpc.NewClient handles the unix scheme via the standard resolver.
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial CRI %q: %w", target, err)
	}
	return &CRIClient{
		conn:    conn,
		images:  runtimeapi.NewImageServiceClient(conn),
		runtime: runtimeapi.NewRuntimeServiceClient(conn),
	}, nil
}

// Close releases the gRPC connection.
func (c *CRIClient) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *CRIClient) ListImages(ctx context.Context) ([]Image, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := c.images.ListImages(ctx, &runtimeapi.ListImagesRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]Image, 0, len(resp.Images))
	for _, img := range resp.Images {
		out = append(out, Image{
			ID:        img.GetId(),
			RepoTags:  img.GetRepoTags(),
			SizeBytes: int64(img.GetSize()),
		})
	}
	return out, nil
}

func (c *CRIClient) ListContainers(ctx context.Context) ([]Container, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// No state filter: exited-but-still-present containers also pin images, so
	// they must keep those images from being pruned.
	resp, err := c.runtime.ListContainers(ctx, &runtimeapi.ListContainersRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(resp.Containers))
	for _, ctr := range resp.Containers {
		c := Container{ImageRef: ctr.GetImageRef()}
		if img := ctr.GetImage(); img != nil {
			c.ImageName = img.GetImage()
		}
		out = append(out, c)
	}
	return out, nil
}

func (c *CRIClient) RemoveImage(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	_, err := c.images.RemoveImage(ctx, &runtimeapi.RemoveImageRequest{
		Image: &runtimeapi.ImageSpec{Image: id},
	})
	return err
}
