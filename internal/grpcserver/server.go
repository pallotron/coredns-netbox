package grpcserver

import (
	"context"
	"net"
	"strings"

	"github.com/pallotron/coredns-netbox/internal/dynamicstore"
	"github.com/pallotron/coredns-netbox/internal/zonemanager"
	pb "github.com/pallotron/coredns-netbox/proto/coredns_netbox/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

// Server wraps a gRPC server with all registered services.
type Server struct {
	grpc *grpc.Server
}

// New creates a Server with DynamicZoneService and ControlService registered.
// token: bearer token required on all calls; empty disables auth.
// mgr may be nil in tests (GetStatus returns zero for active_zones).
func New(
	token string,
	store dynamicstore.DynamicStore,
	cache *NetboxCache,
	st *StatusTracker,
	mergeSignal chan<- struct{},
	netboxSignal chan<- struct{},
	mgr *zonemanager.Manager,
) *Server {
	interceptor := authInterceptor(token)
	gs := grpc.NewServer(grpc.UnaryInterceptor(interceptor))

	pb.RegisterDynamicZoneServiceServer(gs, &dynamicZoneService{
		store:       store,
		cache:       cache,
		mergeSignal: mergeSignal,
	})
	pb.RegisterControlServiceServer(gs, &controlService{
		st:           st,
		store:        store,
		mgr:          mgr,
		mergeSignal:  mergeSignal,
		netboxSignal: netboxSignal,
	})
	reflection.Register(gs)

	return &Server{grpc: gs}
}

// Serve starts accepting connections on lis. Blocks until Stop is called.
func (s *Server) Serve(lis net.Listener) {
	_ = s.grpc.Serve(lis)
}

// Stop gracefully stops the server.
func (s *Server) Stop() {
	s.grpc.GracefulStop()
}

// authInterceptor returns a unary interceptor that validates the bearer token.
// If token is empty, all calls are allowed through.
func authInterceptor(token string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if token == "" {
			return handler(ctx, req)
		}
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing metadata")
		}
		vals := md.Get("authorization")
		if len(vals) == 0 || !strings.EqualFold(vals[0], "bearer "+token) {
			return nil, status.Error(codes.Unauthenticated, "invalid token")
		}
		return handler(ctx, req)
	}
}
