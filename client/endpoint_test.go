package client_test

import (
	"errors"
	"testing"

	"github.com/hivellm/thunder-go/client"
)

func TestParseEndpoint(t *testing.T) {
	cfg := client.Standard().WithScheme("myapp").WithPort(9000)

	cases := []struct {
		name     string
		in       string
		wantHost string
		wantPort int
	}{
		{"scheme with port", "myapp://host:1234", "host", 1234},
		{"scheme default port", "myapp://host", "host", 9000},
		{"scheme trailing slash", "myapp://host:1234/", "host", 1234},
		{"bare host port", "example.com:5555", "example.com", 5555},
		{"ipv6 bracket port", "[::1]:6543", "::1", 6543},
		{"ipv6 scheme default", "myapp://[fe80::1]", "fe80::1", 9000},
		{"bare ipv6 no port", "::1", "::1", -1}, // rejected: bare needs a port
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ep, err := client.ParseEndpoint(tc.in, cfg)
			if tc.wantPort == -1 {
				if err == nil {
					t.Fatalf("expected error for %q, got %+v", tc.in, ep)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ep.Host != tc.wantHost || ep.Port != tc.wantPort {
				t.Fatalf("got %s:%d want %s:%d", ep.Host, ep.Port, tc.wantHost, tc.wantPort)
			}
		})
	}
}

func TestParseEndpointRejectsHTTP(t *testing.T) {
	cfg := client.Standard().WithScheme("myapp")
	for _, url := range []string{"http://localhost:8080", "https://host/path"} {
		_, err := client.ParseEndpoint(url, cfg)
		var ce *client.ConnectionError
		if !errors.As(err, &ce) {
			t.Fatalf("%s: expected ConnectionError, got %v", url, err)
		}
		if !contains(ce.Msg, "RPC-only") || !contains(ce.Msg, "HTTP client") {
			t.Fatalf("%s: message should point to the HTTP client: %s", url, ce.Msg)
		}
	}
}

func TestParseEndpointRejectsMismatchedScheme(t *testing.T) {
	cfg := client.Standard().WithScheme("myapp")
	_, err := client.ParseEndpoint("other://host:1", cfg)
	var ce *client.ConnectionError
	if !errors.As(err, &ce) {
		t.Fatalf("expected ConnectionError, got %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
