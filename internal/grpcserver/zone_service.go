package grpcserver

import (
	"context"
	"strings"

	"github.com/pallotron/coredns-netbox/internal/dynamicstore"
	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	pb "github.com/pallotron/coredns-netbox/proto/coredns_netbox/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type dynamicZoneService struct {
	pb.UnimplementedDynamicZoneServiceServer
	store       dynamicstore.DynamicStore
	cache       *NetboxCache
	mergeSignal chan<- struct{}
}

func (s *dynamicZoneService) signal() {
	select {
	case s.mergeSignal <- struct{}{}:
	default:
	}
}

func (s *dynamicZoneService) CreateZone(_ context.Context, req *pb.CreateZoneRequest) (*pb.CreateZoneResponse, error) {
	if err := s.store.CreateZone(req.Name); err != nil {
		return nil, status.Errorf(codes.Internal, "create zone: %v", err)
	}
	s.signal()
	return &pb.CreateZoneResponse{}, nil
}

func (s *dynamicZoneService) DeleteZone(_ context.Context, req *pb.DeleteZoneRequest) (*pb.DeleteZoneResponse, error) {
	if err := s.store.DeleteZone(req.Name); err != nil {
		return nil, status.Errorf(codes.Internal, "delete zone: %v", err)
	}
	s.signal()
	return &pb.DeleteZoneResponse{}, nil
}

func (s *dynamicZoneService) ListZones(_ context.Context, _ *pb.ListZonesRequest) (*pb.ListZonesResponse, error) {
	return &pb.ListZonesResponse{Names: s.store.ListZones()}, nil
}

func (s *dynamicZoneService) UpsertRecord(_ context.Context, req *pb.UpsertRecordRequest) (*pb.UpsertRecordResponse, error) {
	if !req.Force && s.cache.HasRecord(req.Record.DnsName) {
		return nil, status.Errorf(codes.AlreadyExists, "record %q exists in Netbox; use force=true to override", req.Record.DnsName)
	}
	rec := protoToIPRecord(req.Record)
	if err := s.store.UpsertRecords(req.Zone, []netboxclient.IPRecord{rec}); err != nil {
		return nil, status.Errorf(codes.Internal, "upsert record: %v", err)
	}
	s.signal()
	return &pb.UpsertRecordResponse{}, nil
}

func (s *dynamicZoneService) DeleteRecord(_ context.Context, req *pb.DeleteRecordRequest) (*pb.DeleteRecordResponse, error) {
	if err := s.store.DeleteRecords(req.Zone, []string{req.DnsName}); err != nil {
		return nil, status.Errorf(codes.Internal, "delete record: %v", err)
	}
	s.signal()
	return &pb.DeleteRecordResponse{}, nil
}

func (s *dynamicZoneService) ListRecords(_ context.Context, req *pb.ListRecordsRequest) (*pb.ListRecordsResponse, error) {
	recs := s.store.GetRecords(req.Zone)
	pbRecs := make([]*pb.Record, len(recs))
	for i, r := range recs {
		pbRecs[i] = ipRecordToProto(r)
	}
	return &pb.ListRecordsResponse{Records: pbRecs}, nil
}

func (s *dynamicZoneService) BatchUpsert(_ context.Context, req *pb.BatchUpsertRequest) (*pb.BatchUpsertResponse, error) {
	if !req.Force {
		var conflicts []string
		for _, zr := range req.ZoneRecords {
			for _, r := range zr.Records {
				if s.cache.HasRecord(r.DnsName) {
					conflicts = append(conflicts, r.DnsName)
				}
			}
		}
		if len(conflicts) > 0 {
			return nil, status.Errorf(codes.AlreadyExists,
				"records conflict with Netbox (use force=true to override): %s",
				strings.Join(conflicts, ", "))
		}
	}
	batch := make(map[string][]netboxclient.IPRecord, len(req.ZoneRecords))
	for _, zr := range req.ZoneRecords {
		recs := make([]netboxclient.IPRecord, len(zr.Records))
		for i, r := range zr.Records {
			recs[i] = protoToIPRecord(r)
		}
		batch[zr.Zone] = recs
	}
	if err := s.store.BatchUpsert(batch); err != nil {
		return nil, status.Errorf(codes.Internal, "batch upsert: %v", err)
	}
	s.signal()
	return &pb.BatchUpsertResponse{}, nil
}

func (s *dynamicZoneService) BatchDelete(_ context.Context, req *pb.BatchDeleteRequest) (*pb.BatchDeleteResponse, error) {
	if err := s.store.BatchDelete(req.Zone, req.DnsNames); err != nil {
		return nil, status.Errorf(codes.Internal, "batch delete: %v", err)
	}
	s.signal()
	return &pb.BatchDeleteResponse{}, nil
}

func protoToIPRecord(r *pb.Record) netboxclient.IPRecord {
	return netboxclient.IPRecord{
		DNSName: r.DnsName,
		Address: r.Address,
		Family:  int(r.Family),
		TTL:     r.Ttl,
	}
}

func ipRecordToProto(r netboxclient.IPRecord) *pb.Record {
	return &pb.Record{
		DnsName: r.DNSName,
		Address: r.Address,
		Family:  int32(r.Family),
		Ttl:     r.TTL,
	}
}
