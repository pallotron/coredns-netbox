// Package nameformat renders canonical and alias FQDNs for device names.
// An ordered list of regexes (first match wins) decomposes a device name
// into named variables; text/template formats render the complete FQDNs.
// See docs/superpowers/specs/2026-07-03-name-templates-cname-aliases-design.md.
package nameformat

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"text/template"
)

// Names holds the rendered FQDNs for one device.
type Names struct {
	Canonical string
	Aliases   []string
}

// Formatter parses device names and renders name templates. A nil *Formatter
// is valid and means the feature is off (every method reports no match).
type Formatter struct {
	parsers    []*regexp.Regexp
	groups     []string // union of capture group names across all parsers
	tmpl       *template.Template
	numAliases int
	domain     string
}

// renderedNameRE matches a plausible rendered FQDN: lowercase dot-separated
// labels of letters/digits/hyphens/underscores, no empty labels, optional
// trailing dot. Deliberately permissive (underscores are common in internal
// DNS) — the goal is catching template bugs (empty output, stray whitespace,
// leading dots), not full RFC validation.
var renderedNameRE = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]*[a-z0-9_])?(\.[a-z0-9_]([a-z0-9_-]*[a-z0-9_])?)*\.?$`)

var funcs = template.FuncMap{
	// alphaPrefix returns the leading alphabetic prefix of s. i is a byte
	// offset; the 'a'..'z' guard ensures only single-byte ASCII runes are
	// consumed, so byte-slicing s[:i] is safe.
	"alphaPrefix": func(s string) string {
		for i, c := range s {
			if c < 'a' || c > 'z' {
				return s[:i]
			}
		}
		return s
	},
	"upper": strings.ToUpper,
	"lower": strings.ToLower,
}

// Builtin variable names; capture groups may not shadow them.
var reservedVars = map[string]bool{"name": true, "domain": true}

// New compiles parsers and templates. Returns (nil, nil) when parsers and
// canonical are both empty (feature off). domain is exposed to templates as
// {{.domain}}. zone, when non-empty, is registered as the named sub-template
// "zone" for use as {{template "zone" .}}.
func New(parsers []string, canonical string, aliases []string, zone, domain string) (*Formatter, error) {
	if len(parsers) == 0 && canonical == "" {
		return nil, nil
	}
	if len(parsers) == 0 {
		return nil, fmt.Errorf("nameformat: DEVICE_NAME_PARSERS is required when name formats are configured")
	}
	if canonical == "" {
		return nil, fmt.Errorf("nameformat: NAME_FORMAT_CANONICAL is required when parsers are configured")
	}

	f := &Formatter{domain: domain, numAliases: len(aliases)}

	seen := map[string]bool{}
	for i, p := range parsers {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("nameformat: parser %d: %w", i, err)
		}
		for _, g := range re.SubexpNames() {
			if g == "" {
				continue
			}
			if reservedVars[g] {
				return nil, fmt.Errorf("nameformat: parser %d: capture group %q is reserved", i, g)
			}
			if !seen[g] {
				seen[g] = true
				f.groups = append(f.groups, g)
			}
		}
		f.parsers = append(f.parsers, re)
	}

	root := template.New("nameformat").Funcs(funcs).Option("missingkey=error")
	if zone != "" {
		if _, err := root.New("zone").Parse(zone); err != nil {
			return nil, fmt.Errorf("nameformat: zone template: %w", err)
		}
	}
	if _, err := root.New("canonical").Parse(canonical); err != nil {
		return nil, fmt.Errorf("nameformat: canonical template: %w", err)
	}
	for i, a := range aliases {
		if _, err := root.New(aliasName(i)).Parse(a); err != nil {
			return nil, fmt.Errorf("nameformat: alias template %d: %w", i, err)
		}
	}
	f.tmpl = root
	return f, nil
}

func aliasName(i int) string { return fmt.Sprintf("alias%d", i) }

// SplitLines splits a newline-separated config value into trimmed,
// non-empty entries. Used for DEVICE_NAME_PARSERS and NAME_FORMAT_ALIASES.
func SplitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// MatchIndex returns the index of the first parser matching deviceName, or
// -1 if none match (or the formatter is nil). Matching is case-insensitive
// (the name is lowercased first).
func (f *Formatter) MatchIndex(deviceName string) int {
	if f == nil {
		return -1
	}
	dl := strings.ToLower(deviceName)
	for i, re := range f.parsers {
		if re.MatchString(dl) {
			return i
		}
	}
	return -1
}

// Format renders the canonical and alias FQDNs for deviceName. ok is false
// when no parser matches (callers fall back to legacy naming). Aliases that
// render identical to the canonical, or fail to render, are skipped.
func (f *Formatter) Format(deviceName string) (Names, bool) {
	if f == nil {
		return Names{}, false
	}
	dl := strings.ToLower(deviceName)

	var vars map[string]string
	for _, re := range f.parsers {
		m := re.FindStringSubmatch(dl)
		if m == nil {
			continue
		}
		vars = make(map[string]string, len(f.groups)+2)
		for _, g := range f.groups {
			vars[g] = "" // absent groups render empty so {{if .g}} guards work
		}
		for i, g := range re.SubexpNames() {
			if g != "" {
				vars[g] = m[i]
			}
		}
		break
	}
	if vars == nil {
		return Names{}, false
	}
	vars["name"] = dl
	vars["domain"] = f.domain

	canonical, err := f.execute("canonical", vars)
	if err != nil {
		slog.Error("nameformat: canonical template failed; falling back to legacy naming",
			"device", deviceName, "err", err)
		return Names{}, false
	}
	canonical = strings.ToLower(canonical)
	if !renderedNameRE.MatchString(canonical) {
		slog.Error("nameformat: canonical template rendered an invalid name; falling back to legacy naming",
			"device", deviceName, "rendered", canonical)
		return Names{}, false
	}

	names := Names{Canonical: canonical}
	for i := 0; i < f.numAliases; i++ {
		alias, err := f.execute(aliasName(i), vars)
		if err != nil {
			slog.Error("nameformat: alias template failed; skipping alias",
				"device", deviceName, "alias_index", i, "err", err)
			continue
		}
		alias = strings.ToLower(alias)
		if !renderedNameRE.MatchString(alias) {
			slog.Error("nameformat: alias template rendered an invalid name; skipping alias",
				"device", deviceName, "alias_index", i, "rendered", alias)
			continue
		}
		if alias == canonical {
			slog.Debug("nameformat: alias renders identical to canonical; skipping",
				"device", deviceName, "alias", alias)
			continue
		}
		names.Aliases = append(names.Aliases, alias)
	}
	return names, true
}

func (f *Formatter) execute(name string, vars map[string]string) (string, error) {
	var b strings.Builder
	if err := f.tmpl.ExecuteTemplate(&b, name, vars); err != nil {
		return "", err
	}
	return b.String(), nil
}

// BMCName derives the BMC form of a rendered FQDN by inserting "-bmc"
// before the first dot — i.e. appending it to the hostname label. This is a
// fixed rule (not templated); it preserves the legacy <name>-bmc convention.
func BMCName(fqdn string) string {
	if i := strings.Index(fqdn, "."); i >= 0 {
		return fqdn[:i] + "-bmc" + fqdn[i:]
	}
	return fqdn + "-bmc"
}
