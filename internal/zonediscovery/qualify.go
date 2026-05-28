package zonediscovery

import "strings"

// QualifyDNSName ensures a DNS name is fully qualified with the given domain
// suffix. If the name already ends with .<domainSuffix> (with or without a
// trailing dot) it is returned unchanged. Otherwise .<domainSuffix> is
// appended, covering names entered in NetBox without a domain (e.g.
// "mycluster-ufm" → "mycluster-ufm.example.org").
func QualifyDNSName(name, domainSuffix string) string {
	if name == "" || domainSuffix == "" {
		return name
	}
	bare := strings.TrimSuffix(name, ".")
	if strings.HasSuffix(bare, "."+domainSuffix) {
		return name
	}
	return bare + "." + domainSuffix
}
