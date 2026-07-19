package client

import (
	"fmt"
	"strconv"
	"strings"
)

// Endpoint is a resolved RPC endpoint: host plus concrete port.
type Endpoint struct {
	// Host is a host name or IP literal (IPv6 without brackets).
	Host string
	// Port is the concrete port — explicit, or the config's DefaultPort.
	Port int
}

// ParseEndpoint parses an endpoint string against the application's Config
// (CLT-070). Accepted forms:
//
//   - scheme://host[:port] where scheme is config.Scheme; a missing port
//     resolves to config.DefaultPort (CLT-071).
//   - bare host:port (RPC implied).
//   - [v6::addr]:port / scheme://[v6::addr][:port] for IPv6 literals.
//
// http:// and https:// are rejected: Thunder is RPC-only. Parse failures use
// the ConnectionError class — an endpoint that cannot be parsed is an endpoint
// that cannot be dialed.
func ParseEndpoint(text string, config Config) (Endpoint, error) {
	text = strings.TrimSpace(text)
	if idx := strings.Index(text, "://"); idx != -1 {
		scheme := strings.ToLower(text[:idx])
		rest := text[idx+3:]
		if scheme == "http" || scheme == "https" {
			return Endpoint{}, invalidEndpoint(
				"'%s' is an HTTP URL and Thunder is RPC-only - use the application's "+
					"HTTP client for REST endpoints, or pass an RPC endpoint such as "+
					"'scheme://host:port' or bare 'host:port'", text)
		}
		if scheme != config.Scheme {
			return Endpoint{}, invalidEndpoint(
				"endpoint scheme '%s' does not match this client's configured scheme "+
					"'%s' - set the scheme on the Config, or use bare 'host:port'",
				scheme, config.Scheme)
		}
		rest = strings.TrimSuffix(rest, "/")
		if strings.Contains(rest, "/") {
			return Endpoint{}, invalidEndpoint(
				"endpoint '%s' must not carry a path - expected %s://host[:port]", text, scheme)
		}
		host, port, err := splitHostPort(rest)
		if err != nil {
			return Endpoint{}, err
		}
		if port < 0 {
			return Endpoint{Host: host, Port: int(config.DefaultPort)}, nil
		}
		return Endpoint{Host: host, Port: port}, nil
	}
	host, port, err := splitHostPort(text)
	if err != nil {
		return Endpoint{}, err
	}
	if port < 0 {
		return Endpoint{}, invalidEndpoint(
			"bare endpoint '%s' needs an explicit port ('host:port') - only "+
				"scheme-prefixed endpoints resolve a registry default port", text)
	}
	return Endpoint{Host: host, Port: port}, nil
}

// splitHostPort splits host[:port], handling bracketed IPv6 literals. A
// missing port is signalled by a returned port of -1.
func splitHostPort(s string) (host string, port int, err error) {
	if s == "" {
		return "", 0, invalidEndpoint("endpoint host is empty")
	}
	if strings.HasPrefix(s, "[") {
		inner := s[1:]
		close := strings.IndexByte(inner, ']')
		if close == -1 {
			return "", 0, invalidEndpoint("unterminated '[' in endpoint host '%s'", s)
		}
		h := inner[:close]
		tail := inner[close+1:]
		if h == "" {
			return "", 0, invalidEndpoint("endpoint host is empty")
		}
		if tail == "" {
			return h, -1, nil
		}
		if !strings.HasPrefix(tail, ":") {
			return "", 0, invalidEndpoint("expected ':port' after ']' in endpoint '%s'", s)
		}
		p, err := parsePort(tail[1:], s)
		if err != nil {
			return "", 0, err
		}
		return h, p, nil
	}
	idx := strings.LastIndexByte(s, ':')
	if idx == -1 {
		return s, -1, nil
	}
	head := s[:idx]
	if strings.Contains(head, ":") {
		// More than one ':' without brackets: an IPv6 literal, no port.
		return s, -1, nil
	}
	if head == "" {
		return "", 0, invalidEndpoint("endpoint host is empty")
	}
	p, err := parsePort(s[idx+1:], s)
	if err != nil {
		return "", 0, err
	}
	return head, p, nil
}

func parsePort(port, whole string) (int, error) {
	value, err := strconv.Atoi(port)
	if err != nil || value < 0 || value > 65535 || !isAllDigits(port) {
		return 0, invalidEndpoint("invalid port '%s' in endpoint '%s'", port, whole)
	}
	return value, nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func invalidEndpoint(format string, args ...any) *ConnectionError {
	return &ConnectionError{Msg: fmt.Sprintf(format, args...)}
}
