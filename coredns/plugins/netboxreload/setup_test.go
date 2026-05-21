package netboxreload

import (
	"testing"
	"time"

	"github.com/coredns/caddy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantDir      string
		wantPort     string
		wantInterval time.Duration
	}{
		{
			name:         "explicit dir, port, and reload interval",
			input:        `netboxreload { directory /zones grpc_port :9054 reload 30s }`,
			wantDir:      "/zones",
			wantPort:     ":9054",
			wantInterval: 30 * time.Second,
		},
		{
			name:         "disable polling with reload 0s",
			input:        `netboxreload { directory /zones reload 0s }`,
			wantDir:      "/zones",
			wantPort:     ":8054",
			wantInterval: 0,
		},
		{
			name:         "defaults",
			input:        `netboxreload`,
			wantDir:      "/zones",
			wantPort:     ":8054",
			wantInterval: 60 * time.Second,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := caddy.NewTestController("dns", tc.input)
			p, err := parseConfig(c)
			require.NoError(t, err)
			assert.Equal(t, tc.wantDir, p.Dir)
			assert.Equal(t, tc.wantPort, p.Port)
			assert.Equal(t, tc.wantInterval, p.PollInterval)
		})
	}
}
