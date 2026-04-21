package http

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// fakeResolver lets us inject DNS answers without hitting the network.
// A nil `err` with no matching host returns net.ErrNotExist-ish behavior.
type fakeResolver struct {
	answers map[string][]net.IP
	err     error
}

func (f fakeResolver) LookupIP(host string) ([]net.IP, error) {
	if f.err != nil {
		return nil, f.err
	}
	ips, ok := f.answers[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return ips, nil
}

func TestValidateTarget_AcceptsPublicHTTPS(t *testing.T) {
	r := fakeResolver{answers: map[string][]net.IP{
		"example.com": {net.ParseIP("93.184.216.34")},
	}}
	if err := ValidateTarget("https://example.com/path?q=1", r); err != nil {
		t.Errorf("unexpected err: %v", err)
	}
}

func TestValidateTarget_RejectsEmpty(t *testing.T) {
	err := ValidateTarget("", nil)
	if !errors.Is(err, ErrInvalidURL) {
		t.Errorf("err = %v, want ErrInvalidURL", err)
	}
}

func TestValidateTarget_RejectsOverLength(t *testing.T) {
	long := "https://example.com/" + strings.Repeat("a", MaxURLLength)
	err := ValidateTarget(long, nil)
	if !errors.Is(err, ErrInvalidURL) {
		t.Errorf("err = %v, want ErrInvalidURL", err)
	}
}

func TestValidateTarget_RejectsDisallowedSchemes(t *testing.T) {
	cases := []string{
		"javascript:alert(1)",
		"data:text/html,<script>",
		"file:///etc/passwd",
		"ftp://example.com/",
		"gopher://example.com/",
		"ws://example.com/",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			// Resolver won't be called when scheme check fails; pass nil.
			err := ValidateTarget(raw, nil)
			if !errors.Is(err, ErrInvalidURL) {
				t.Errorf("err = %v, want ErrInvalidURL", err)
			}
		})
	}
}

func TestValidateTarget_RejectsMissingHost(t *testing.T) {
	err := ValidateTarget("https://", nil)
	if !errors.Is(err, ErrInvalidURL) {
		t.Errorf("err = %v, want ErrInvalidURL", err)
	}
}

func TestValidateTarget_RejectsLiteralPrivateIPs(t *testing.T) {
	r := fakeResolver{} // won't be consulted for IP literals
	cases := []string{
		"http://127.0.0.1/",
		"http://10.0.0.1/",
		"http://192.168.1.1/",
		"http://172.16.5.4/",
		"http://169.254.169.254/", // AWS/GCP metadata endpoint
		"http://0.0.0.0/",
		"http://[::1]/",
		"http://[fe80::1]/",
		"http://100.64.0.1/", // CGNAT
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			err := ValidateTarget(raw, r)
			if !errors.Is(err, ErrInvalidURL) {
				t.Errorf("err = %v, want ErrInvalidURL", err)
			}
		})
	}
}

func TestValidateTarget_RejectsHostThatResolvesPrivate(t *testing.T) {
	r := fakeResolver{answers: map[string][]net.IP{
		"internal.corp": {net.ParseIP("10.0.0.5")},
	}}
	err := ValidateTarget("https://internal.corp/", r)
	if !errors.Is(err, ErrInvalidURL) {
		t.Errorf("err = %v, want ErrInvalidURL", err)
	}
}

// DNS rebinding defense: any private IP among the answers disqualifies.
func TestValidateTarget_RejectsMixedPublicAndPrivateAnswers(t *testing.T) {
	r := fakeResolver{answers: map[string][]net.IP{
		"mixed.example": {
			net.ParseIP("93.184.216.34"),
			net.ParseIP("10.0.0.5"),
		},
	}}
	err := ValidateTarget("https://mixed.example/", r)
	if !errors.Is(err, ErrInvalidURL) {
		t.Errorf("err = %v, want ErrInvalidURL", err)
	}
}

func TestValidateTarget_RejectsUnresolvableHost(t *testing.T) {
	r := fakeResolver{} // no answers -> NXDOMAIN-style
	err := ValidateTarget("https://does.not.exist.invalid/", r)
	if !errors.Is(err, ErrInvalidURL) {
		t.Errorf("err = %v, want ErrInvalidURL", err)
	}
}

func TestValidateTarget_NilResolverSkipsDNSCheck(t *testing.T) {
	// Literal private IP is still rejected (that's checked before DNS),
	// but a hostname with nil resolver bypasses the lookup entirely.
	// This mode exists for unit tests that don't want DNS in the loop.
	if err := ValidateTarget("https://example.com/", nil); err != nil {
		t.Errorf("unexpected err with nil resolver: %v", err)
	}
	// Literal private IP still rejected even with nil resolver.
	if err := ValidateTarget("http://127.0.0.1/", nil); !errors.Is(err, ErrInvalidURL) {
		t.Errorf("literal loopback should still be rejected, got %v", err)
	}
}

func TestValidateTarget_SchemeCaseInsensitive(t *testing.T) {
	r := fakeResolver{answers: map[string][]net.IP{
		"example.com": {net.ParseIP("93.184.216.34")},
	}}
	if err := ValidateTarget("HTTPS://example.com/", r); err != nil {
		t.Errorf("HTTPS (uppercase) should be accepted: %v", err)
	}
}
