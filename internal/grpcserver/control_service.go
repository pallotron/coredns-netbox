package grpcserver

import (
	"context"
	"log/slog"

	"github.com/pallotron/coredns-netbox/internal/dynamicstore"
	"github.com/pallotron/coredns-netbox/internal/shrinkguard"
	"github.com/pallotron/coredns-netbox/internal/zonemanager"
	pb "github.com/pallotron/coredns-netbox/proto/coredns_netbox/v1"
)

type controlService struct {
	pb.UnimplementedControlServiceServer
	st           *StatusTracker
	store        dynamicstore.DynamicStore
	mgr          *zonemanager.Manager // may be nil in tests
	mergeSignal  chan<- struct{}
	netboxSignal chan<- struct{}
	// shrinkOverride is armed by ForceNetboxPoll(allow_shrink=true); may be
	// nil in tests.
	shrinkOverride *shrinkguard.Override
}

func (s *controlService) ForceNetboxPoll(_ context.Context, req *pb.ForceNetboxPollRequest) (*pb.ForceNetboxPollResponse, error) {
	// Arm before signalling: the poll this call triggers must be the one
	// that sees the consent.
	if req.GetAllowShrink() && s.shrinkOverride != nil {
		slog.Warn("operator armed a single-use shrink-guard override via ForceNetboxPoll")
		s.shrinkOverride.Arm()
	}
	select {
	case s.netboxSignal <- struct{}{}:
	default:
	}
	return &pb.ForceNetboxPollResponse{}, nil
}

func (s *controlService) ForceMergeWrite(_ context.Context, _ *pb.ForceMergeWriteRequest) (*pb.ForceMergeWriteResponse, error) {
	select {
	case s.mergeSignal <- struct{}{}:
	default:
	}
	return &pb.ForceMergeWriteResponse{}, nil
}

func (s *controlService) GetStatus(_ context.Context, _ *pb.GetStatusRequest) (*pb.GetStatusResponse, error) {
	netboxPoll, mergeWrite, staleness := s.st.Get()

	var activeZones int32
	if s.mgr != nil {
		activeZones = int32(len(s.mgr.Zones()))
	}

	var dynamicCount int32
	for _, zone := range s.store.ListZones() {
		dynamicCount += int32(len(s.store.GetRecords(zone)))
	}

	return &pb.GetStatusResponse{
		LastNetboxPollUnix:   netboxPoll.UnixMilli(),
		LastMergeWriteUnix:   mergeWrite.UnixMilli(),
		ActiveZones:          activeZones,
		DynamicRecordCount:   dynamicCount,
		ZoneStalenessSeconds: staleness,
	}, nil
}
