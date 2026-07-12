package netboxwebhook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSecret = "test-webhook-secret-123"

// realSignature is the exact X-Hook-Signature Netbox sent for testdata/created.json,
// captured live against a Netbox v4.6.0 instance (see issue #27 comment).
const realSignature = "f756f462809c6cb1a7b4d3a47a7ea6216a3f7ca84aa411f5bb2bc95439831868ae7a89675dcbefe9d07aff8647d53a31b6d75a0f9c720c60660e44946e8eb968"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return raw
}

func TestVerifySignature_RealNetboxCapture(t *testing.T) {
	body := readFixture(t, "created.json")
	assert.True(t, verifySignature(testSecret, body, realSignature))
}

func TestVerifySignature_WrongSecret(t *testing.T) {
	body := readFixture(t, "created.json")
	assert.False(t, verifySignature("wrong-secret", body, realSignature))
}

func TestVerifySignature_TamperedBody(t *testing.T) {
	body := readFixture(t, "created.json")
	body = append(body, ' ') // any mutation invalidates the signature
	assert.False(t, verifySignature(testSecret, body, realSignature))
}

func TestVerifySignature_MissingHeader(t *testing.T) {
	body := readFixture(t, "created.json")
	assert.False(t, verifySignature(testSecret, body, ""))
}
