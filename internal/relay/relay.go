//go:generate protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative relay.proto
package relay

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/csnewman/localflux/internal/cluster"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const bufferSize = 64 * 1024

type Callbacks interface {
	Completed(msg string, dur time.Duration)

	State(msg string, detail string, start time.Time)

	Success(detail string)

	Info(msg string)

	Warn(msg string)

	Error(msg string)
}

type Client struct {
	logger      *slog.Logger
	relayClient RelayClient
}

func NewClient(logger *slog.Logger) *Client {
	return &Client{
		logger: logger,
	}
}

func (c *Client) Run(ctx context.Context, name string, b64 string, cb Callbacks) error {
	cb.Info(fmt.Sprintf("Relaying to %q", name))

	var config *restclient.Config

	if b64 != "" {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return fmt.Errorf("config base64 decoding failed: %w", err)
		}

		cfg, err := clientcmd.Load(raw)
		if err != nil {
			return fmt.Errorf("config from bytes failed: %w", err)
		}

		config, err = clientcmd.NewNonInteractiveClientConfig(
			*cfg,
			name,
			&clientcmd.ConfigOverrides{
				CurrentContext: name,
			},
			nil,
		).ClientConfig()
		if err != nil {
			return err
		}
	} else {
		loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(),
			&clientcmd.ConfigOverrides{
				CurrentContext: name,
			},
		)

		var err error

		config, err = loader.ClientConfig()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
	}

	client, err := cluster.NewK8sClientFromConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create k8s client: %w", err)
	}

	relayConn, err := grpc.NewClient(
		"127.0.0.1",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			c.logger.Info("Finding relay pod")

			podList, err := client.ClientSet().CoreV1().Pods(cluster.LFNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "deployment=relay",
			})
			if err != nil {
				return nil, fmt.Errorf("failed to list pods: %w", err)
			}

			var podName string

			for _, pod := range podList.Items {
				if pod.Status.Phase != corev1.PodRunning {
					continue
				}

				podName = pod.Name
			}

			if podName == "" {
				c.logger.Warn("Failed to find any active relay pods!")

				return nil, fmt.Errorf("failed to find relay pod")
			}

			c.logger.Info("Found relay pod", "pod", podName)

			return client.PortForward(cluster.LFNamespace, podName, 8080)
		}),
	)
	if err != nil {
		return fmt.Errorf("failed to create grpc client: %w", err)
	}

	c.relayClient = NewRelayClient(relayConn)

	addr, err := netip.ParseAddrPort("0.0.0.0:8081")
	if err != nil {
		return fmt.Errorf("failed to parse address: %w", err)
	}

	// TODO: replace
	return c.relayTCP(ctx, addr, "10.102.39.180:8080")

	return nil
}

func (c *Client) relayTCP(ctx context.Context, bind netip.AddrPort, remote string) error {
	lis, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(bind))
	if err != nil {
		return fmt.Errorf("could not listen: %w", err)
	}

	go func() {
		<-ctx.Done()
		_ = lis.Close()
	}()

	for {
		tcpConn, err := lis.AcceptTCP()
		if err != nil {
			return fmt.Errorf("could not accept connection: %w", err)
		}

		go func() {
			c.logger.Info("Relaying TCP", "bind", bind)

			if err := relayTCPClientInstance(ctx, c.relayClient, tcpConn, remote); err != nil {
				c.logger.Info("Relaying failed", "bind", bind, "err", err)
			}
		}()
	}
}

func relayTCPClientInstance(ctx context.Context, rc RelayClient, tcpConn *net.TCPConn, remote string) error {
	defer tcpConn.Close()

	conn, err := rc.Relay(ctx)
	if err != nil {
		return fmt.Errorf("failed to relay: %w", err)
	}

	if err := conn.Send(&RelayRequest{
		Message: &RelayRequest_Start{
			Start: &RelayRequestStart{
				Network: RelayNetwork_TCP,
				Address: remote,
			},
		},
	}); err != nil {
		return fmt.Errorf("failed to send start: %w", err)
	}

	grp, gctx := errgroup.WithContext(ctx)

	go func() {
		<-gctx.Done()
		_ = tcpConn.Close()
	}()

	grp.Go(func() error {
		defer func() {
			_ = tcpConn.CloseRead()

			_ = conn.Send(&RelayRequest{
				Message: &RelayRequest_Close{
					Close: RelayClose_CLOSE_WRITE,
				},
			})
		}()

		for {
			buffer := make([]byte, bufferSize)

			read, err := tcpConn.Read(buffer)
			if errors.Is(err, io.EOF) {
				return nil
			} else if err != nil {
				return fmt.Errorf("could not read: %w", err)
			}

			if err := conn.Send(&RelayRequest{
				Message: &RelayRequest_Data{
					Data: &RelayData{
						Data: buffer[:read],
					},
				},
			}); err != nil {
				return fmt.Errorf("failed to relay read: %w", err)
			}
		}
	})

	grp.Go(func() error {
		for {
			resp, err := conn.Recv()
			if err != nil {
				return fmt.Errorf("failed to receive: %w", err)
			}

			switch m := resp.GetMessage().(type) {
			case *RelayResponse_Data:
				if _, err := tcpConn.Write(m.Data.Data); err != nil {
					return fmt.Errorf("failed to write: %w", err)
				}
			case *RelayResponse_Close:
				switch m.Close {
				case RelayClose_CLOSE_FULL:
					_ = tcpConn.Close()
				case RelayClose_CLOSE_WRITE:
					_ = tcpConn.CloseWrite()
				case RelayClose_CLOSE_READ:
					_ = tcpConn.CloseRead()
				default:
					return fmt.Errorf("%w: unexpected close type", ErrBadRequest)
				}
			default:
				return fmt.Errorf("%w: unexpected message type", ErrBadRequest)
			}
		}
	})

	return grp.Wait()
}
