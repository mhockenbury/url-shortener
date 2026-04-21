// Package http holds the chi-based router, HTTP handlers, and URL-input
// validation. Handlers depend on the shortener, storage, and events
// packages via small interfaces defined here.
package http

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// MaxURLLength caps the long_url the API will accept. Matches README §1
// non-functional requirements. Keeping it at the validation layer rather
// than the DB schema so we return a clean 400 rather than a DB error.
const MaxURLLength = 2048

// allowedSchemes are the only URL schemes we accept. Anything else (data:,
// javascript:, file:, ftp:, custom schemes) risks open-redirect-to-XSS or
// abuse of the redirect as a protocol launcher.
var allowedSchemes = map[string]struct{}{
	"http":  {},
	"https": {},
}

// ErrInvalidURL is returned by ValidateTarget for any validation failure.
// Wrap with %w to preserve the specific reason while letting callers map
// the whole family to 400 Bad Request.
var ErrInvalidURL = errors.New("invalid target URL")

// Resolver abstracts DNS lookup so tests can inject results without hitting
// the network. The production path uses net.DefaultResolver via defaultResolver.
type Resolver interface {
	LookupIP(host string) ([]net.IP, error)
}

// defaultResolver wraps net.LookupIP to satisfy the Resolver interface.
type defaultResolver struct{}

func (defaultResolver) LookupIP(host string) ([]net.IP, error) {
	return net.LookupIP(host)
}

// DefaultResolver is the production Resolver. Exported so tests in other
// packages can substitute it if they need to.
var DefaultResolver Resolver = defaultResolver{}

// ValidateTarget enforces the rules documented in docs/tradeoffs.md under
// "URL validation strictness": scheme allowlist, length cap, and a DNS-based
// SSRF guard rejecting RFC1918 / loopback / link-local / multicast hosts.
//
// The resolver parameter is used for the SSRF guard; pass DefaultResolver
// for production. Nil is treated as "skip the DNS check" — useful for unit
// tests that assert the non-DNS checks in isolation.
func ValidateTarget(raw string, resolver Resolver) error {
	if raw == "" {
		return fmt.Errorf("%w: empty", ErrInvalidURL)
	}
	if len(raw) > MaxURLLength {
		return fmt.Errorf("%w: length %d exceeds max %d", ErrInvalidURL, len(raw), MaxURLLength)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: parse: %v", ErrInvalidURL, err)
	}
	if _, ok := allowedSchemes[strings.ToLower(u.Scheme)]; !ok {
		return fmt.Errorf("%w: scheme %q not allowed", ErrInvalidURL, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: missing host", ErrInvalidURL)
	}

	return checkHostSafe(host, resolver)
}

// checkHostSafe rejects hosts whose IPs fall into disallowed ranges. Literal
// IPs are always checked, even if resolver is nil — skipping DNS for tests
// must never bypass an obvious `http://127.0.0.1/` attempt. If the host is
// not a literal IP and resolver is nil, DNS is skipped.
//
// When resolver is non-nil we check every returned address so a DNS answer
// with mixed public+private IPs (a DNS-rebinding vector) is caught.
func checkHostSafe(host string, resolver Resolver) error {
	if ip := net.ParseIP(host); ip != nil {
		if disallowed(ip) {
			return fmt.Errorf("%w: host %q resolves to disallowed address %s", ErrInvalidURL, host, ip)
		}
		return nil
	}
	if resolver == nil {
		return nil
	}

	ips, err := resolver.LookupIP(host)
	if err != nil {
		return fmt.Errorf("%w: resolve %q: %v", ErrInvalidURL, host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("%w: host %q resolved to no addresses", ErrInvalidURL, host)
	}
	for _, ip := range ips {
		if disallowed(ip) {
			return fmt.Errorf("%w: host %q resolves to disallowed address %s", ErrInvalidURL, host, ip)
		}
	}
	return nil
}

// disallowed reports whether ip is in a range we refuse to shorten.
// Any one of these being true is enough to reject.
func disallowed(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() ||
		isCGNAT(ip)
}

// isCGNAT checks 100.64.0.0/10 (carrier-grade NAT, RFC 6598). net.IP doesn't
// expose a method for this and it's a real SSRF vector on some networks.
func isCGNAT(ip net.IP) bool {
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return cgnat.Contains(ip)
}
