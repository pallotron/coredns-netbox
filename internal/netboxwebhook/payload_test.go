package netboxwebhook

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePayload_Created(t *testing.T) {
	p, err := parsePayload(readFixture(t, "created.json"))
	require.NoError(t, err)
	assert.Equal(t, "created", p.Event)
	assert.Equal(t, "ipam.ipaddress", p.ObjectType)
	assert.Nil(t, p.Snapshots.PreChange)

	rec, err := recordFromPayload(p)
	require.NoError(t, err)
	assert.Equal(t, "webhook-test-1.mycompany.com", rec.DNSName)
	assert.Equal(t, "10.99.99.5", rec.Address)
}

func TestParsePayload_UpdatedRename(t *testing.T) {
	p, err := parsePayload(readFixture(t, "updated_rename.json"))
	require.NoError(t, err)
	assert.Equal(t, "updated", p.Event)
	require.NotNil(t, p.Snapshots.PreChange)
	assert.Equal(t, "webhook-test-1.mycompany.com", p.Snapshots.PreChange.DNSName)

	rec, err := recordFromPayload(p)
	require.NoError(t, err)
	assert.Equal(t, "webhook-test-1-renamed.mycompany.com", rec.DNSName)
}

func TestParsePayload_Deleted(t *testing.T) {
	p, err := parsePayload(readFixture(t, "deleted.json"))
	require.NoError(t, err)
	assert.Equal(t, "deleted", p.Event)
	require.NotNil(t, p.Snapshots.PreChange)
	assert.Equal(t, "webhook-test-1-renamed.mycompany.com", p.Snapshots.PreChange.DNSName)
}

func TestParsePayload_Malformed(t *testing.T) {
	_, err := parsePayload([]byte(`not-json`))
	assert.Error(t, err)
}
