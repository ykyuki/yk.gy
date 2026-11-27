// Package privacy decides which personal data may be written to logs.
package privacy

import (
	"net/netip"
	"strings"
)

// noIPCountries are the countries whose visitors' IP addresses are never
// logged: the EU (27), the other EEA states where the GDPR applies (IS, LI,
// NO) and the United Kingdom (UK GDPR). ISO 3166-1 alpha-2 codes.
var noIPCountries = map[string]bool{
	"AT": true, "BE": true, "BG": true, "HR": true, "CY": true, "CZ": true,
	"DK": true, "EE": true, "FI": true, "FR": true, "DE": true, "GR": true,
	"HU": true, "IE": true, "IT": true, "LV": true, "LT": true, "LU": true,
	"MT": true, "NL": true, "PL": true, "PT": true, "RO": true, "SK": true,
	"SI": true, "ES": true, "SE": true,
	"IS": true, "LI": true, "NO": true,
	"GB": true,
}

// IPAllowed reports whether the IP address of a visitor from country may be
// logged. Unknown countries (geolocation failed) are treated like the EU so
// that EU/UK visitors are never logged by accident.
func IPAllowed(country string) bool {
	c := strings.ToUpper(strings.TrimSpace(country))
	if c == "" || c == "**" || c == "--" {
		return false
	}
	return !noIPCountries[c]
}

// ClientIP extracts the client IP from a remote address ("ip" or "ip:port"),
// or "" when it cannot be parsed.
func ClientIP(remoteAddr string) string {
	if a, err := netip.ParseAddr(remoteAddr); err == nil {
		return a.Unmap().String()
	}
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return ap.Addr().Unmap().String()
	}
	return ""
}
