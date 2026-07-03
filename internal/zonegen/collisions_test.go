package zonegen

import (
	"testing"

	"github.com/pallotron/coredns-netbox/internal/netboxclient"
	"github.com/stretchr/testify/assert"
)

func cname(name, target, device string) netboxclient.IPRecord {
	return netboxclient.IPRecord{
		DNSName: name, Type: netboxclient.RecordTypeCNAME,
		CNAMETarget: target, DeviceName: device,
	}
}

func arec(name, addr string) netboxclient.IPRecord {
	return netboxclient.IPRecord{DNSName: name, Address: addr, Family: 4}
}

func TestResolveCNAMECollisions(t *testing.T) {
	tests := []struct {
		name string
		in   []netboxclient.IPRecord
		want []netboxclient.IPRecord
	}{
		{
			name: "clean cname kept",
			in:   []netboxclient.IPRecord{arec("host1.example.org", "10.0.0.1"), cname("alias1.example.org", "host1.example.org", "dev1")},
			want: []netboxclient.IPRecord{arec("host1.example.org", "10.0.0.1"), cname("alias1.example.org", "host1.example.org", "dev1")},
		},
		{
			name: "cname vs address data drops the cname, keeps all address records",
			in: []netboxclient.IPRecord{
				arec("host1.example.org", "10.0.0.1"),
				arec("host1.example.org", "10.0.0.2"), // multi-A at one name is legal
				cname("host1.example.org", "other.example.org", "dev1"),
			},
			want: []netboxclient.IPRecord{arec("host1.example.org", "10.0.0.1"), arec("host1.example.org", "10.0.0.2")},
		},
		{
			name: "ambiguous cnames all dropped, never pick a winner",
			in: []netboxclient.IPRecord{
				cname("alias1.example.org", "host1.example.org", "dev1"),
				cname("alias1.example.org", "host2.example.org", "dev2"),
			},
			want: nil,
		},
		{
			name: "exact duplicate cnames deduped to one",
			in: []netboxclient.IPRecord{
				cname("alias1.example.org", "host1.example.org", "dev1"),
				cname("alias1.example.org", "host1.example.org", "dev1"),
			},
			want: []netboxclient.IPRecord{cname("alias1.example.org", "host1.example.org", "dev1")},
		},
		{
			name: "name matching is case-insensitive and trailing-dot-insensitive",
			in: []netboxclient.IPRecord{
				arec("Host1.example.org", "10.0.0.1"),
				cname("host1.example.org.", "other.example.org", "dev1"),
			},
			want: []netboxclient.IPRecord{arec("Host1.example.org", "10.0.0.1")},
		},
		{
			name: "duplicate-target dedup is case-insensitive on target",
			in: []netboxclient.IPRecord{
				cname("alias1.example.org", "HOST1.example.org", "dev1"),
				cname("alias1.example.org", "host1.example.org", "dev1"),
			},
			want: []netboxclient.IPRecord{cname("alias1.example.org", "HOST1.example.org", "dev1")},
		},
		{
			name: "empty target dropped",
			in:   []netboxclient.IPRecord{cname("alias1.example.org", "", "dev1")},
			want: nil,
		},
		{
			name: "apex cname dropped: would coexist with SOA and NS",
			in:   []netboxclient.IPRecord{cname("example.org", "host1.other.org", "dev1")},
			want: nil,
		},
		{
			name: "ambiguity drop wins over duplicate dedup",
			in: []netboxclient.IPRecord{
				cname("alias1.example.org", "host1.example.org", "dev1"),
				cname("alias1.example.org", "host1.example.org", "dev1"),
				cname("alias1.example.org", "host2.example.org", "dev2"),
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveCNAMECollisions(tt.in, "example.org")
			assert.Equal(t, tt.want, got)
		})
	}
}
