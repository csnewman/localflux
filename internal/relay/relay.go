//go:generate protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative relay.proto
package relay

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/csnewman/localflux/internal/cluster"
	"github.com/csnewman/localflux/internal/deployment/v1alpha1"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	ctlscheme "k8s.io/kubectl/pkg/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	client      *cluster.K8sClient
	statuses    map[string]*Status
}

func NewClient(logger *slog.Logger) *Client {
	return &Client{
		logger:   logger,
		statuses: make(map[string]*Status),
	}
}

func (c *Client) Run(ctx context.Context, name string, b64 string, cb Callbacks) error {
	cb.Info(fmt.Sprintf("Relaying to %q", name))

	var loader clientcmd.ClientConfig

	if b64 != "" {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return fmt.Errorf("config base64 decoding failed: %w", err)
		}

		cfg, err := clientcmd.Load(raw)
		if err != nil {
			return fmt.Errorf("config from bytes failed: %w", err)
		}

		loader = clientcmd.NewNonInteractiveClientConfig(
			*cfg,
			name,
			&clientcmd.ConfigOverrides{
				CurrentContext: name,
			},
			nil,
		)
	} else {
		loader = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(),
			&clientcmd.ConfigOverrides{
				CurrentContext: name,
			},
		)

	}

	config, err := loader.ClientConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	rawConfig, err := loader.RawConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	c.client, err = cluster.NewK8sClientFromConfig(config, rawConfig)
	if err != nil {
		return fmt.Errorf("failed to create k8s client: %w", err)
	}

	relayConn, err := grpc.NewClient(
		"127.0.0.1",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			c.logger.Info("Finding relay pod")

			podList, err := c.client.ClientSet().CoreV1().Pods(cluster.LFNamespace).List(ctx, metav1.ListOptions{
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

			return c.client.PortForward(cluster.LFNamespace, podName, 8080)
		}),
	)
	if err != nil {
		return fmt.Errorf("failed to create grpc client: %w", err)
	}

	c.relayClient = NewRelayClient(relayConn)

	if err := c.reconcile(ctx, cb); err != nil {
		return fmt.Errorf("reconciliation failed: %w", err)
	}

	t := time.NewTicker(time.Second * 10)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := c.reconcile(ctx, cb); err != nil {
				return fmt.Errorf("reconciliation failed: %w", err)
			}
		}
	}
}

func (c *Client) reconcile(ctx context.Context, cb Callbacks) error {
	var deployments v1alpha1.DeploymentList

	if err := c.client.Controller().List(ctx, &deployments, client.InNamespace(cluster.LFNamespace)); err != nil {
		return fmt.Errorf("failed to list deployments: %w", err)
	}

	forwards := make(map[string]*v1alpha1.PortForward)

	for _, deployment := range deployments.Items {
		for _, forward := range deployment.PortForward {
			key := pfKey(forward)

			forwards[key] = forward
		}
	}

	for _, key := range slices.Collect(maps.Keys(c.statuses)) {
		_, ok := forwards[key]
		if ok {
			continue
		}

		status, ok := c.statuses[key]
		if !ok {
			continue
		}

		if status.active.Load() {
			cb.Info(fmt.Sprintf("Removing forward: %s", key))

			status.cancel()

			continue
		}

		delete(c.statuses, key)
	}

	for key, forward := range forwards {
		status, ok := c.statuses[key]
		if ok && status.active.Load() {
			continue
		}

		cb.Info(fmt.Sprintf("Creating forward: %s", key))

		forwardCtx, forwardCancel := context.WithCancel(ctx)
		status = &Status{
			cancel: forwardCancel,
		}

		status.active.Store(true)

		go func() {
			if err := c.runForward(forwardCtx, forward, status); err != nil {
				c.logger.Warn("Port forward error", "key", key, "err", err)

				cb.Warn(fmt.Sprintf("Port forward error: %v", err.Error()))
			}
		}()

		c.statuses[key] = status

	}
	return nil
}

func (c *Client) runForward(ctx context.Context, forward *v1alpha1.PortForward, status *Status) error {
	defer func() {
		status.active.Store(false)
	}()

	defer status.cancel()

	var remoteResolver func(ctx context.Context) (string, error)

	switch strings.ToLower(forward.Kind) {
	case "service":
		remoteResolver = func(ctx context.Context) (string, error) {
			service, err := c.client.ClientSet().CoreV1().Services(forward.Namespace).Get(ctx, forward.Name, metav1.GetOptions{})
			if err != nil {
				return "", fmt.Errorf("failed to get service: %w", err)
			}

			return service.Spec.ClusterIP + ":" + strconv.Itoa(forward.Port), nil
		}
	default:
		remoteResolver = func(ctx context.Context) (string, error) {
			builder := resource.NewBuilder(c.client).
				WithScheme(ctlscheme.Scheme, ctlscheme.Scheme.PrioritizedVersionsAllGroups()...).
				ContinueOnError().
				NamespaceParam(forward.Namespace).
				DefaultNamespace().
				ResourceNames("pods", forward.Kind+"/"+forward.Name)

			obj, err := builder.Do().Object()
			if err != nil {
				return "", fmt.Errorf("failed to find resource: %w", err)
			}

			forwardablePod, err := polymorphichelpers.AttachablePodForObjectFn(c.client, obj, time.Second*10)
			if err != nil {
				return "", fmt.Errorf("failed to find attachable pod: %w", err)
			}

			return forwardablePod.Status.PodIP + ":" + strconv.Itoa(forward.Port), nil
		}
	}

	localPort := forward.Port
	if forward.LocalPort != nil {
		localPort = *forward.LocalPort
	}

	local, err := netip.ParseAddrPort("0.0.0.0:" + strconv.Itoa(localPort))
	if err != nil {
		return fmt.Errorf("failed to parse address: %w", err)
	}

	switch strings.ToLower(forward.Network) {
	case "tcp":
		return c.relayTCP(ctx, local, remoteResolver)
	default:
		return fmt.Errorf("unsupported network: %s", forward.Network)
	}
}

type Status struct {
	active atomic.Bool
	cancel func()
}

func pfKey(pf *v1alpha1.PortForward) string {
	k := "kind=" + pf.Kind + " ns=" + pf.Namespace + " name=" + pf.Name + " net=" + pf.Network + " port=" + strconv.Itoa(pf.Port)

	if pf.LocalPort != nil {
		k += " local=" + strconv.Itoa(*pf.LocalPort)
	}

	return k
}

func (c *Client) relayTCP(ctx context.Context, bind netip.AddrPort, remoteResolver func(ctx context.Context) (string, error)) error {
	lis, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(bind))
	if err != nil {
		return fmt.Errorf("could not listen: %w", err)
	}

	defer lis.Close()

	go func() {
		<-ctx.Done()
		_ = lis.Close()
	}()

	remote, err := remoteResolver(ctx)
	if err != nil {
		return fmt.Errorf("could not resolve remote address: %w", err)
	}

	lastResolve := time.Now()

	for {
		tcpConn, err := lis.AcceptTCP()
		if err != nil {
			return fmt.Errorf("could not accept connection: %w", err)
		}

		if time.Since(lastResolve) >= time.Second {
			remote, err = remoteResolver(ctx)
			if err != nil {
				_ = tcpConn.CloseWrite()

				return fmt.Errorf("could not resolve remote address: %w", err)
			}

			lastResolve = time.Now()
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
