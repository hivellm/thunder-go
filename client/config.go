// Package client is the multiplexed Thunder RPC client (SPEC-003) plus the
// protocol Config (SPEC-002) and endpoint parsing it drives its behavior
// from. One Client owns one TCP connection and demultiplexes concurrent
// in-flight calls over it by request id.
package client

import "github.com/hivellm/thunder-go/wire"

// Handshake is the handshake style (PRO-001).
type Handshake int

const (
	// HandshakeNone means no RPC-layer handshake at all: the connection is
	// usable immediately.
	HandshakeNone Handshake = iota
	// HandshakeAuthCommand: HELLO optional; AUTH [api_key] / [user, pass] /
	// [password]; pre-auth allowlist PING/HELLO/AUTH/QUIT. Whether a
	// deployment enforces credentials is its own policy, not a dialect: a
	// client with no credentials configured simply sends no AUTH.
	HandshakeAuthCommand
	// HandshakeHelloMandatory: HELLO must be the first frame, carrying
	// credentials. The standard — see Standard.
	HandshakeHelloMandatory
)

// String renders the standard.yaml token for the handshake style.
func (h Handshake) String() string {
	switch h {
	case HandshakeNone:
		return "none"
	case HandshakeAuthCommand:
		return "auth_command"
	case HandshakeHelloMandatory:
		return "hello_mandatory"
	default:
		return "invalid"
	}
}

// HelloStyle is the HELLO payload style (PRO-001).
type HelloStyle int

const (
	// HelloStyleNotUsed: the application has no HELLO command.
	HelloStyleNotUsed HelloStyle = iota
	// HelloStyleArgLess: HELLO with no arguments; the reply is a metadata Map
	// {server, version, proto, id, authenticated}. Credentials travel via
	// AUTH, never inside the HELLO.
	HelloStyleArgLess
	// HelloStyleMapPayload: Map with version, token | api_key, client_name;
	// the reply carries proto and capabilities. The standard.
	HelloStyleMapPayload
)

// String renders the standard.yaml token for the HELLO style.
func (h HelloStyle) String() string {
	switch h {
	case HelloStyleNotUsed:
		return "not_used"
	case HelloStyleArgLess:
		return "arg_less"
	case HelloStyleMapPayload:
		return "map_payload"
	default:
		return "invalid"
	}
}

// PushPolicy is the server-push policy (PRO-001).
type PushPolicy int

const (
	// PushReserved: PUSH_ID reserved: servers refuse it from clients and never
	// emit it. The standard — receiving one poisons the connection (CLT-060).
	PushReserved PushPolicy = iota
	// PushEnabled: push frames flow to the client's push hook.
	PushEnabled
)

// String renders the standard.yaml token for the push policy.
func (p PushPolicy) String() string {
	switch p {
	case PushReserved:
		return "reserved"
	case PushEnabled:
		return "enabled"
	default:
		return "invalid"
	}
}

// ErrorConvention selects which error-string prefix conventions the client
// parses (PRO-014).
type ErrorConvention int

const (
	// ErrorNone: no prefix parsing.
	ErrorNone ErrorConvention = iota
	// ErrorResp3Prefixes: ERR / NOAUTH / WRONGPASS / NOPERM prefixes.
	ErrorResp3Prefixes
	// ErrorBracketCode: leading "[<code>] " machine-readable code.
	ErrorBracketCode
	// ErrorBoth: both conventions composed. The standard — a strict superset.
	ErrorBoth
)

// String renders the standard.yaml token for the error convention.
func (e ErrorConvention) String() string {
	switch e {
	case ErrorNone:
		return "none"
	case ErrorResp3Prefixes:
		return "resp3_prefixes"
	case ErrorBracketCode:
		return "bracket_code"
	case ErrorBoth:
		return "both"
	default:
		return "invalid"
	}
}

// TlsPolicy is the transport-security policy (PRO-001).
type TlsPolicy int

const (
	// TlsOff: plain TCP. The standard default.
	TlsOff TlsPolicy = iota
	// TlsOptional: TLS available behind configuration.
	TlsOptional
	// TlsReserved: config keys reserved; not wired yet.
	TlsReserved
)

// String renders the standard.yaml token for the TLS policy.
func (t TlsPolicy) String() string {
	switch t {
	case TlsOff:
		return "off"
	case TlsOptional:
		return "optional_rustls"
	case TlsReserved:
		return "reserved_config"
	default:
		return "invalid"
	}
}

// Config is one application's protocol configuration (PRO-001). Configs are
// data, never behavior: no config may alter wire bytes (PRO-003) — it selects
// among behaviors Thunder already implements. Construct with Standard and the
// With* builder, or as a plain struct; both are supported.
//
// The zero value is NOT the standard (Go zero-values enums to their first
// constant); use Standard for the family standard.
type Config struct {
	// Scheme is the URL scheme the endpoint parser accepts for this
	// application (PRO-012). Identity — Thunder has no default for it.
	Scheme string
	// DefaultPort is the default RPC port for the scheme (PRO-012).
	DefaultPort uint16
	// Handshake is the handshake style.
	Handshake Handshake
	// HelloStyle is the HELLO payload style.
	HelloStyle HelloStyle
	// Push is the server-push policy.
	Push PushPolicy
	// MaxFrameBytes is the frame cap (WIRE-020).
	MaxFrameBytes int
	// MaxInFlight is the per-connection in-flight request bound (CLT-012).
	MaxInFlight int
	// ErrorCodes is the error-string conventions the client parses.
	ErrorCodes ErrorConvention
	// Tls is the transport-security policy.
	Tls TlsPolicy
}

// Standard is the family standard (pinned by conformance/standard.yaml):
// mandatory HELLO map with proto negotiation and a capabilities reply; the
// [CODE] error superset; 64 MiB frames; 256 in-flight; push reserved; TLS
// off. Scheme is "" and DefaultPort is 0 — identity is the application's to
// supply, and a Config that never sets them is only usable with an explicit
// host:port endpoint.
func Standard() Config {
	return Config{
		Scheme:        "",
		DefaultPort:   0,
		Handshake:     HandshakeHelloMandatory,
		HelloStyle:    HelloStyleMapPayload,
		Push:          PushReserved,
		MaxFrameBytes: wire.DefaultMaxFrameBytes,
		MaxInFlight:   256,
		ErrorCodes:    ErrorBoth,
		Tls:           TlsOff,
	}
}

// WithScheme sets the URL scheme this application answers on (PRO-012).
func (c Config) WithScheme(scheme string) Config { c.Scheme = scheme; return c }

// WithPort sets the default RPC port for the scheme (PRO-012).
func (c Config) WithPort(port uint16) Config { c.DefaultPort = port; return c }

// WithHandshake overrides the handshake style.
func (c Config) WithHandshake(h Handshake) Config { c.Handshake = h; return c }

// WithHelloStyle overrides the HELLO payload style.
func (c Config) WithHelloStyle(h HelloStyle) Config { c.HelloStyle = h; return c }

// WithPush overrides the server-push policy.
func (c Config) WithPush(p PushPolicy) Config { c.Push = p; return c }

// WithMaxFrameBytes overrides the frame cap (WIRE-020).
func (c Config) WithMaxFrameBytes(n int) Config { c.MaxFrameBytes = n; return c }

// WithMaxInFlight overrides the per-connection in-flight bound.
func (c Config) WithMaxInFlight(n int) Config { c.MaxInFlight = n; return c }

// WithErrorCodes overrides the error-string conventions parsed.
func (c Config) WithErrorCodes(e ErrorConvention) Config { c.ErrorCodes = e; return c }

// WithTls overrides the transport-security policy.
func (c Config) WithTls(t TlsPolicy) Config { c.Tls = t; return c }
