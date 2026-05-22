package netboxreload

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/pallotron/coredns-netbox/proto/coredns_netbox/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestGRPCReload(t *testing.T) {
	dir := t.TempDir()
	v1 := `$ORIGIN mycompany.com.
$TTL 300
@ IN SOA ns1.mycompany.com. admin.mycompany.com. (2026052101 3600 900 604800 86400)
@ IN NS ns1.mycompany.com.
host1 IN A 10.0.0.1
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "db.mycompany.com"), []byte(v1), 0o644))
	zones, err := loadZoneDir(dir)
	require.NoError(t, err)
	p := &Plugin{Dir: dir, zones: zones}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := newGRPCServer(p)
	go srv.Serve(lis) //nolint:errcheck
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	client := pb.NewZoneReloadServiceClient(conn)

	// overwrite zone with new content before calling Reload
	v2 := `$ORIGIN mycompany.com.
$TTL 300
@ IN SOA ns1.mycompany.com. admin.mycompany.com. (2026052102 3600 900 604800 86400)
@ IN NS ns1.mycompany.com.
host2 IN A 10.0.0.2
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "db.mycompany.com"), []byte(v2), 0o644))

	_, err = client.Reload(context.Background(), &pb.ZoneReloadRequest{})
	require.NoError(t, err)

	p.mu.RLock()
	defer p.mu.RUnlock()
	assert.Contains(t, p.zones["mycompany.com."].records, "host2.mycompany.com.")
	assert.NotContains(t, p.zones["mycompany.com."].records, "host1.mycompany.com.")
}
