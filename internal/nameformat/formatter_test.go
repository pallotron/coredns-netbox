package nameformat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewValidation(t *testing.T) {
	valid := `^(?P<dc>[a-z0-9]+)-r(?P<rack>[0-9]+)-(?P<role>[a-z]+)-(?P<idx>[0-9]+)$`

	tests := []struct {
		name      string
		parsers   []string
		canonical string
		aliases   []string
		zone      string
		wantErr   string
	}{
		{name: "no parsers and no formats is valid (feature off)"},
		{name: "valid full config", parsers: []string{valid},
			canonical: `{{.name}}.{{.domain}}`, aliases: []string{`{{.role}}{{.idx}}.{{.domain}}`}},
		{name: "invalid regex", parsers: []string{`(`}, canonical: `{{.name}}.{{.domain}}`,
			wantErr: "parser"},
		{name: "parsers without canonical", parsers: []string{valid},
			wantErr: "NAME_FORMAT_CANONICAL is required"},
		{name: "canonical without parsers", canonical: `{{.name}}.{{.domain}}`,
			wantErr: "DEVICE_NAME_PARSERS is required"},
		{name: "reserved capture group name",
			parsers: []string{`^(?P<name>[a-z]+)$`}, canonical: `{{.name}}.{{.domain}}`,
			wantErr: "reserved"},
		{name: "reserved capture group domain",
			parsers: []string{`^(?P<domain>[a-z]+)$`}, canonical: `{{.name}}.{{.domain}}`,
			wantErr: "reserved"},
		{name: "bad canonical template", parsers: []string{valid}, canonical: `{{.name`,
			wantErr: "canonical"},
		{name: "bad alias template", parsers: []string{valid},
			canonical: `{{.name}}.{{.domain}}`, aliases: []string{`{{oops .role}}`},
			wantErr: "alias"},
		{name: "zone subtemplate is usable", parsers: []string{valid},
			canonical: `{{.name}}.{{template "zone" .}}`, zone: `{{.dc}}.{{.domain}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := New(tt.parsers, tt.canonical, tt.aliases, tt.zone, "example.org")
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			if len(tt.parsers) == 0 {
				assert.Nil(t, f, "feature off returns a nil formatter")
			} else {
				assert.NotNil(t, f)
			}
		})
	}
}

func TestSplitLines(t *testing.T) {
	assert.Nil(t, SplitLines(""))
	assert.Equal(t, []string{"a", "b"}, SplitLines("a\nb"))
	assert.Equal(t, []string{"a", "b"}, SplitLines(" a \n\n b \n"))
}

func newTestFormatter(t *testing.T) *Formatter {
	t.Helper()
	f, err := New(
		[]string{
			// standard device with env: dc1-h2a-r101-prod-hv-01
			`^(?P<dc>[a-z0-9]+)-(?P<hall>[a-z]+[0-9][a-z0-9]*)-r(?P<rack>[0-9]+)-(?P<env>prod|mgmt|staging)-(?P<role>[a-z][a-z0-9-]*?)-(?P<idx>[0-9]+)$`,
			// no hall: site01-r101-pdu-left-01
			`^(?P<dc>[a-z0-9]+)-r(?P<rack>[0-9]+)-(?P<role>[a-z][a-z0-9-]*?)-(?P<idx>[0-9]+)$`,
		},
		`{{.name}}.{{template "zone" .}}`,
		[]string{`{{.role}}{{.idx}}-{{.dc}}{{if .hall}}-{{.hall}}{{end}}-r{{.rack}}.{{template "zone" .}}`},
		`{{.dc}}{{if .hall}}-{{alphaPrefix .hall}}{{end}}.{{.domain}}`,
		"example.org",
	)
	require.NoError(t, err)
	return f
}

func TestFormat(t *testing.T) {
	f := newTestFormatter(t)

	t.Run("standard device: canonical, alias, alphaPrefix zone", func(t *testing.T) {
		names, ok := f.Format("dc1-h2a-r101-prod-hv-01")
		require.True(t, ok)
		assert.Equal(t, "dc1-h2a-r101-prod-hv-01.dc1-h.example.org", names.Canonical)
		assert.Equal(t, []string{"hv01-dc1-h2a-r101.dc1-h.example.org"}, names.Aliases)
	})

	t.Run("second parser: absent groups render empty via if-guards", func(t *testing.T) {
		names, ok := f.Format("site01-r101-pdu-left-01")
		require.True(t, ok)
		assert.Equal(t, "site01-r101-pdu-left-01.site01.example.org", names.Canonical)
		assert.Equal(t, []string{"pdu-left01-site01-r101.site01.example.org"}, names.Aliases)
	})

	t.Run("first match wins over later parsers", func(t *testing.T) {
		assert.Equal(t, 0, f.MatchIndex("dc1-h2a-r101-prod-hv-01"))
		assert.Equal(t, 1, f.MatchIndex("site01-r101-pdu-left-01"))
	})

	t.Run("no match falls back", func(t *testing.T) {
		_, ok := f.Format("core-router-wan")
		assert.False(t, ok)
		assert.Equal(t, -1, f.MatchIndex("core-router-wan"))
	})

	t.Run("input is lowercased", func(t *testing.T) {
		names, ok := f.Format("DC1-H2A-R101-PROD-HV-01")
		require.True(t, ok)
		assert.Equal(t, "dc1-h2a-r101-prod-hv-01.dc1-h.example.org", names.Canonical)
	})

	t.Run("nil formatter reports no match", func(t *testing.T) {
		var nilF *Formatter
		_, ok := nilF.Format("anything")
		assert.False(t, ok)
	})
}

func TestFormatAliasEqualsCanonicalSkipped(t *testing.T) {
	f, err := New(
		[]string{`^(?P<dc>[a-z0-9]+)-(?P<role>[a-z]+)-(?P<idx>[0-9]+)$`},
		`{{.name}}.{{.domain}}`,
		[]string{`{{.name}}.{{.domain}}`}, // renders identical to canonical
		"", "example.org",
	)
	require.NoError(t, err)
	names, ok := f.Format("dc1-hv-01")
	require.True(t, ok)
	assert.Empty(t, names.Aliases, "alias identical to canonical is skipped")
}

func TestBMCName(t *testing.T) {
	assert.Equal(t, "host-bmc.example.org", BMCName("host.example.org"))
	assert.Equal(t, "hv01-dc1-r1-bmc.dc1.example.org", BMCName("hv01-dc1-r1.dc1.example.org"))
	assert.Equal(t, "bare-bmc", BMCName("bare"))
}

func TestFormatRejectsInvalidRenderedNames(t *testing.T) {
	// An alias whose conditional renders nothing produces a leading-dot name;
	// a canonical with an embedded space is a template bug. Both must be
	// caught by rendered-name validation, not emitted into zone data.
	f, err := New(
		[]string{`^(?P<dc>[a-z0-9]+)-(?P<role>[a-z]+)-(?P<idx>[0-9]+)$`},
		`{{.name}}.{{.domain}}`,
		[]string{`{{if false}}x{{end}}.{{.domain}}`}, // renders ".example.org" — invalid leading dot
		"", "example.org",
	)
	require.NoError(t, err)
	names, ok := f.Format("dc1-hv-01")
	require.True(t, ok, "canonical is valid; only the alias is broken")
	assert.Empty(t, names.Aliases, "alias rendering a leading-dot name must be skipped")

	fBad, err := New(
		[]string{`^(?P<dc>[a-z0-9]+)-(?P<role>[a-z]+)-(?P<idx>[0-9]+)$`},
		`{{.name}} extra.{{.domain}}`, // embedded space
		nil, "", "example.org",
	)
	require.NoError(t, err)
	_, ok = fBad.Format("dc1-hv-01")
	assert.False(t, ok, "canonical rendering an invalid name must fall back to legacy naming")
}

func TestFormatLowercasesRenderedNames(t *testing.T) {
	f, err := New(
		[]string{`^(?P<dc>[a-z0-9]+)-(?P<role>[a-z]+)-(?P<idx>[0-9]+)$`},
		`{{upper .role}}{{.idx}}.{{.domain}}`,
		nil, "", "example.org",
	)
	require.NoError(t, err)
	names, ok := f.Format("dc1-hv-01")
	require.True(t, ok)
	assert.Equal(t, "hv01.example.org", names.Canonical, "rendered names are normalized to lowercase")
}
