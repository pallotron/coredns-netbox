package zonediscovery

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestQualifyDNSName(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		domainSuffix string
		want         string
	}{
		{
			name:         "bare hostname gets domain appended",
			input:        "mycluster-ufm",
			domainSuffix: "example.org",
			want:         "mycluster-ufm.example.org",
		},
		{
			name:         "already qualified name is unchanged",
			input:        "mycluster-ufm.example.org",
			domainSuffix: "example.org",
			want:         "mycluster-ufm.example.org",
		},
		{
			name:         "fully qualified with trailing dot is unchanged",
			input:        "mycluster-ufm.example.org.",
			domainSuffix: "example.org",
			want:         "mycluster-ufm.example.org.",
		},
		{
			name:         "multi-label name without domain gets domain appended",
			input:        "block-storage.mydevice-prod-lb-01",
			domainSuffix: "example.org",
			want:         "block-storage.mydevice-prod-lb-01.example.org",
		},
		{
			name:         "multi-label name already qualified is unchanged",
			input:        "block-storage.mydevice-prod-lb-01.example.org",
			domainSuffix: "example.org",
			want:         "block-storage.mydevice-prod-lb-01.example.org",
		},
		{
			name:         "external domain gets suffix appended",
			input:        "host.other.com",
			domainSuffix: "example.org",
			want:         "host.other.com.example.org",
		},
		{
			name:         "empty name is unchanged",
			input:        "",
			domainSuffix: "example.org",
			want:         "",
		},
		{
			name:         "empty domain suffix is unchanged",
			input:        "mycluster-ufm",
			domainSuffix: "",
			want:         "mycluster-ufm",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := QualifyDNSName(tt.input, tt.domainSuffix)
			assert.Equal(t, tt.want, got)
		})
	}
}
