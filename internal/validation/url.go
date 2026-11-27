// Package validation normalizes and validates user supplied URLs before they
// are shortened. It never performs network I/O: the service only redirects,
// it never fetches the target, so the checks here are about refusing
// obviously internal / abusive destinations.
package validation

import (
	"errors"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// MaxURLLength is the maximum accepted length of a URL in bytes.
const MaxURLLength = 2048

// Error codes returned to API clients. They are stable identifiers; the UI
// maps them to Japanese messages.
var (
	ErrEmpty        = &Error{Code: "empty", Msg: "URLが入力されていません"}
	ErrTooLong      = &Error{Code: "too_long", Msg: "URLが長すぎます（最大2048文字）"}
	ErrInvalid      = &Error{Code: "invalid_url", Msg: "URLの形式が正しくありません"}
	ErrScheme       = &Error{Code: "unsupported_scheme", Msg: "http:// または https:// で始まるURLのみ登録できます"}
	ErrUserinfo     = &Error{Code: "userinfo_not_allowed", Msg: "ユーザー名・パスワードを含むURLは登録できません"}
	ErrPrivateHost  = &Error{Code: "private_host", Msg: "localhost・プライベートIPなど内部向けのURLは登録できません"}
	ErrRecursive    = &Error{Code: "recursive", Msg: "このサービスの短縮URLを再度短縮することはできません"}
	ErrBlockedHost  = &Error{Code: "blocked_host", Msg: "他の短縮URLサービスのURLは登録できません"}
	ErrInvalidPort  = &Error{Code: "invalid_port", Msg: "ポート番号が正しくありません"}
	ErrInvalidHost  = &Error{Code: "invalid_host", Msg: "ホスト名が正しくありません"}
	ErrControlChars = &Error{Code: "invalid_characters", Msg: "URLに使用できない文字が含まれています"}
)

// Error is a validation failure with a machine readable code.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return "validation: " + e.Code }

// Options configures validation.
type Options struct {
	// ShortHosts are the hosts this service is served on (yk.gy, dev.yk.gy...).
	// Any URL pointing at them, or at a subdomain of them, is rejected to
	// prevent recursive shortening.
	ShortHosts []string
	// BlockedHosts are other hosts (e.g. third-party shorteners) that are
	// rejected, including their subdomains.
	BlockedHosts []string
}

// internalSuffixes are DNS names that resolve (or commonly resolve) to
// internal addresses and must never be targets.
var internalSuffixes = []string{
	"localhost", "local", "localdomain", "internal", "intranet", "lan",
	"home", "corp", "private", "test", "invalid", "example", "onion",
	"home.arpa", "in-addr.arpa", "ip6.arpa",
	// wildcard DNS services that map names to arbitrary (incl. private) IPs
	"nip.io", "sslip.io", "xip.io", "localtest.me", "lvh.me", "vcap.me",
	"lacolhost.com",
}

// specialPrefixes are non-public IP ranges not fully covered by netip's
// IsPrivate/IsLoopback/... helpers.
var specialPrefixes = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24",
	"192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
	"224.0.0.0/4", "240.0.0.0/4", "255.255.255.255/32",
	"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48",
	"100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "fc00::/7",
	"fe80::/10", "fec0::/10", "ff00::/8",
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

var idnaProfile = idna.New(
	idna.MapForLookup(),
	idna.BidiRule(),
	idna.ValidateLabels(true),
	idna.StrictDomainName(false), // allow '_' which appears in some real hosts
	idna.Transitional(false),
)

// Normalize validates raw and returns a normalized absolute URL string.
func Normalize(raw string, opt Options) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrEmpty
	}
	if len(raw) > MaxURLLength {
		return "", ErrTooLong
	}
	if !utf8.ValidString(raw) {
		return "", ErrControlChars
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f || r == ' ' || r == ' ' || r == ' ' {
			return "", ErrControlChars
		}
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", ErrInvalid
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", ErrScheme
	}
	if u.Opaque != "" || u.Host == "" {
		return "", ErrInvalid
	}
	if u.User != nil {
		return "", ErrUserinfo
	}

	host := u.Hostname()
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") { // "http://example.com:/"
		return "", ErrInvalidPort
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", ErrInvalidPort
		}
		port = strconv.Itoa(n)
	}

	asciiHost, ipHost, err := normalizeHost(host)
	if err != nil {
		return "", err
	}

	if ipHost.IsValid() {
		if !isPublicIP(ipHost) {
			return "", ErrPrivateHost
		}
	} else {
		if isInternalName(asciiHost) {
			return "", ErrPrivateHost
		}
		if matchesAny(asciiHost, opt.ShortHosts) {
			return "", ErrRecursive
		}
		if matchesAny(asciiHost, opt.BlockedHosts) {
			return "", ErrBlockedHost
		}
	}

	hostPart := asciiHost
	if ipHost.IsValid() && ipHost.Is6() {
		hostPart = "[" + ipHost.String() + "]"
	}
	if port != "" && !isDefaultPort(scheme, port) {
		hostPart += ":" + port
	}

	u.Scheme = scheme
	u.Host = hostPart
	if u.Path == "" && u.RawPath == "" {
		u.Path = "/"
	}
	out := u.String()
	if len(out) > MaxURLLength {
		return "", ErrTooLong
	}
	return out, nil
}

func isDefaultPort(scheme, port string) bool {
	return scheme == "http" && port == "80" || scheme == "https" && port == "443"
}

// normalizeHost returns the lower-case ASCII (punycode) host, and if the host
// is any form of IP literal (including legacy inet_aton forms such as
// "2130706433" or "0x7f.1") the parsed address.
func normalizeHost(host string) (string, netip.Addr, error) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return "", netip.Addr{}, ErrInvalidHost
	}
	// IPv6 literal (url.Hostname strips the brackets). Zones are rejected.
	if strings.Contains(host, ":") {
		if strings.Contains(host, "%") {
			return "", netip.Addr{}, ErrPrivateHost
		}
		a, err := netip.ParseAddr(host)
		if err != nil {
			return "", netip.Addr{}, ErrInvalidHost
		}
		return a.String(), a.Unmap(), nil
	}
	if a, ok := parseLegacyIPv4(host); ok {
		return a.String(), a, nil
	}
	ascii, err := idnaProfile.ToASCII(host)
	if err != nil || ascii == "" || len(ascii) > 253 {
		return "", netip.Addr{}, ErrInvalidHost
	}
	labels := strings.Split(ascii, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 {
			return "", netip.Addr{}, ErrInvalidHost
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return "", netip.Addr{}, ErrInvalidHost
			}
		}
	}
	// A purely numeric last label is not a valid public TLD and is how some
	// resolvers interpret partial IPs; refuse it.
	if last := labels[len(labels)-1]; isAllDigits(last) {
		return "", netip.Addr{}, ErrInvalidHost
	}
	return ascii, netip.Addr{}, nil
}

// parseLegacyIPv4 parses dotted IPv4 in the permissive inet_aton syntax that
// browsers accept: 1-4 parts, each decimal, octal (leading 0) or hex (0x).
func parseLegacyIPv4(s string) (netip.Addr, bool) {
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return netip.Addr{}, false
	}
	vals := make([]uint64, len(parts))
	for i, p := range parts {
		if p == "" {
			return netip.Addr{}, false
		}
		base := 10
		digits := p
		switch {
		case strings.HasPrefix(p, "0x") || strings.HasPrefix(p, "0X"):
			base, digits = 16, p[2:]
			if digits == "" {
				digits = "0"
			}
		case len(p) > 1 && p[0] == '0':
			base, digits = 8, p[1:]
		}
		v, err := strconv.ParseUint(digits, base, 64)
		if err != nil {
			return netip.Addr{}, false
		}
		vals[i] = v
	}
	var n uint64
	last := len(vals) - 1
	for i := 0; i < last; i++ {
		if vals[i] > 255 {
			return netip.Addr{}, false
		}
		n |= vals[i] << (8 * uint(3-i))
	}
	if vals[last] >= 1<<(8*uint(4-last)) {
		return netip.Addr{}, false
	}
	n |= vals[last]
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}), true
}

func isPublicIP(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsMulticast() || a.IsUnspecified() || a.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range specialPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

func isInternalName(host string) bool {
	if !strings.Contains(host, ".") {
		return true // single-label names (intranet hosts)
	}
	return matchesAny(host, internalSuffixes)
}

// matchesAny reports whether host equals one of list or is a subdomain of it.
func matchesAny(host string, list []string) bool {
	for _, h := range list {
		h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
		if h == "" {
			continue
		}
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// AsError returns the *Error inside err, if any.
func AsError(err error) (*Error, bool) {
	var ve *Error
	ok := errors.As(err, &ve)
	return ve, ok
}
