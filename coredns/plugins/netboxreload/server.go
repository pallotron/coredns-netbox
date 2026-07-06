package netboxreload

import (
	"context"
	"log/slog"
	"net"

	pb "github.com/pallotron/coredns-netbox/proto/coredns_netbox/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

type grpcReloadServer struct {
	pb.UnimplementedZoneReloadServiceServer
	plugin *Plugin
}

// Reload re-reads zone files from disk and atomically swaps the in-memory
// zone map. Auth is intentionally absent: this endpoint listens only on a
// cluster-internal port (not exposed outside the pod/node) so network
// isolation is the security boundary.
func (s *grpcReloadServer) Reload(_ context.Context, _ *pb.ZoneReloadRequest) (*pb.ZoneReloadResponse, error) {
	if err := s.plugin.reloadZones("grpc"); err != nil {
		slog.Error("netboxreload: zone reload failed", "err", err)
		return nil, status.Errorf(codes.Internal, "reload: %v", err)
	}
	slog.Info("netboxreload: zones reloaded via gRPC")
	return &pb.ZoneReloadResponse{}, nil
}

func newGRPCServer(p *Plugin) *grpc.Server {
	gs := grpc.NewServer()
	pb.RegisterZoneReloadServiceServer(gs, &grpcReloadServer{plugin: p})
	reflection.Register(gs)
	return gs
}

// startGRPC starts the gRPC server on p.Port. Logs and returns on error.
func (p *Plugin) startGRPC() {
	lis, err := net.Listen("tcp", p.Port)
	if err != nil {
		slog.Error("netboxreload: gRPC listen failed", "port", p.Port, "err", err)
		return
	}
	slog.Info("netboxreload: gRPC server listening", "addr", lis.Addr())
	p.mu.Lock()
	p.grpcServer = newGRPCServer(p)
	srv := p.grpcServer
	p.mu.Unlock()
	if err := srv.Serve(lis); err != nil {
		slog.Error("netboxreload: gRPC server exited", "err", err)
	}
}

// stopGRPC gracefully stops the gRPC server if it was started.
func (p *Plugin) stopGRPC() {
	p.mu.RLock()
	srv := p.grpcServer
	p.mu.RUnlock()
	if srv != nil {
		srv.GracefulStop()
	}
}
