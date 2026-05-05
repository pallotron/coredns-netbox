package grpcserver

import (
	"context"

	"github.com/pallotron/coredns-netbox/internal/dynamicstore"
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
}

func (s *controlService) ForceNetboxPoll(_ context.Context, _ *pb.ForceNetboxPollRequest) (*pb.ForceNetboxPollResponse, error) {
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
