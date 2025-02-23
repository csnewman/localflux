package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var ErrBadRequest = errors.New("bad request")

type Server struct {
	UnimplementedRelayServer
	logger *slog.Logger
}

func NewServer(logger *slog.Logger) *Server {
	return &Server{
		logger: logger,
	}
}

func (s *Server) Run(context context.Context) error {
	s.logger.Info("Starting relay server")

	srv := grpc.NewServer()
	RegisterRelayServer(srv, s)

	lis, err := net.Listen("tcp", "0.0.0.0:8080")
	if err != nil {
		return fmt.Errorf("could not listen on port 8080: %w", err)
	}

	go func() {
		<-context.Done()
		_ = lis.Close()
	}()

	return srv.Serve(lis)
}

func (s *Server) Relay(g grpc.BidiStreamingServer[RelayRequest, RelayResponse]) error {
	initial, err := g.Recv()
	if err != nil {
		return err
	}

	start := initial.GetStart()
	if start == nil {
		return fmt.Errorf("%w: no start", ErrBadRequest)
	}

	addr, err := netip.ParseAddrPort(start.Address)
	if err != nil {
		return fmt.Errorf("failed to parse address: %w", err)
	}

	switch start.Network {
	case RelayNetwork_TCP:
		s.logger.Info("Relaying TCP", "dest", addr)

		if err := relayTCPServer(g, addr); err != nil {
			s.logger.Info("Relaying TCP failed", "dest", addr, "err", err)

			return err
		}

		return nil

	case RelayNetwork_UDP:
		return status.Error(codes.Unimplemented, "udp relaying not supported yet")

	default:
		return fmt.Errorf("%w: unsupported network: %s", ErrBadRequest, start.Network)
	}
}

func relayTCPServer(g grpc.BidiStreamingServer[RelayRequest, RelayResponse], addr netip.AddrPort) error {
	tcpConn, err := net.DialTCP("tcp", nil, net.TCPAddrFromAddrPort(addr))
	if err != nil {
		return fmt.Errorf("could not dial: %w", err)
	}

	defer tcpConn.Close()

	grp, gctx := errgroup.WithContext(g.Context())

	go func() {
		<-gctx.Done()
		_ = tcpConn.Close()
	}()

	grp.Go(func() error {
		defer func() {
			_ = tcpConn.CloseRead()

			_ = g.Send(&RelayResponse{
				Message: &RelayResponse_Close{
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

			if err := g.Send(&RelayResponse{
				Message: &RelayResponse_Data{
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
			resp, err := g.Recv()
			if err != nil {
				return fmt.Errorf("failed to receive: %w", err)
			}

			switch m := resp.GetMessage().(type) {
			case *RelayRequest_Data:
				if _, err := tcpConn.Write(m.Data.Data); err != nil {
					return fmt.Errorf("failed to write: %w", err)
				}
			case *RelayRequest_Close:
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
