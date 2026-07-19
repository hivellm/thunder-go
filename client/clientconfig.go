package client

import "time"

// CredentialKind is the kind of handshake credential (CLT-002).
type CredentialKind int

const (
	// CredToken is a bearer token (token key under HELLO_MANDATORY).
	CredToken CredentialKind = iota
	// CredAPIKey is an API key (api_key key under HELLO_MANDATORY, single-arg
	// AUTH under AUTH_COMMAND).
	CredAPIKey
	// CredUserPass is a user + password (AUTH [user, pass] under AUTH_COMMAND).
	CredUserPass
)

// Credentials are the handshake credentials (CLT-002). Auth state is
// per-connection and sticky — there are no per-call credentials (CLT-003).
// Build via TokenCredentials / APIKeyCredentials / UserPassCredentials.
type Credentials struct {
	Kind    CredentialKind
	Secrets []string
}

// TokenCredentials builds bearer-token credentials.
func TokenCredentials(token string) *Credentials {
	return &Credentials{Kind: CredToken, Secrets: []string{token}}
}

// APIKeyCredentials builds API-key credentials.
func APIKeyCredentials(apiKey string) *Credentials {
	return &Credentials{Kind: CredAPIKey, Secrets: []string{apiKey}}
}

// UserPassCredentials builds user + password credentials.
func UserPassCredentials(user, password string) *Credentials {
	return &Credentials{Kind: CredUserPass, Secrets: []string{user, password}}
}

// ClientConfig is the per-client dialing policy (SPEC-003) — distinct from
// Config, which describes the protocol one application speaks (SPEC-002).
// The zero value is usable but has zero timeouts; prefer DefaultClientConfig.
type ClientConfig struct {
	// ConnectTimeout is the TCP connect timeout (CLT-001).
	ConnectTimeout time.Duration
	// CallTimeout is the default per-call timeout (CLT-020); override per call
	// with CallWithTimeout.
	CallTimeout time.Duration
	// Credentials are the handshake credentials, when the protocol config
	// wants them. nil means no AUTH / no credential in the HELLO map.
	Credentials *Credentials
	// ClientName is the client identifier sent in the HELLO map
	// (HELLO_MANDATORY). Empty falls back to DefaultClientName.
	ClientName string
}

// DefaultClientConfig is the default dialing policy: connect timeout 10 s
// (CLT-001), per-call timeout 30 s (CLT-020), no credentials.
func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		ConnectTimeout: 10 * time.Second,
		CallTimeout:    30 * time.Second,
	}
}

// WithConnectTimeout sets the TCP connect timeout.
func (c ClientConfig) WithConnectTimeout(d time.Duration) ClientConfig {
	c.ConnectTimeout = d
	return c
}

// WithCallTimeout sets the default per-call timeout.
func (c ClientConfig) WithCallTimeout(d time.Duration) ClientConfig {
	c.CallTimeout = d
	return c
}

// WithCredentials sets the handshake credentials.
func (c ClientConfig) WithCredentials(creds *Credentials) ClientConfig {
	c.Credentials = creds
	return c
}

// WithClientName sets the client identifier sent in the HELLO map.
func (c ClientConfig) WithClientName(name string) ClientConfig {
	c.ClientName = name
	return c
}

// HandshakeInfo is what the handshake learned about a connection (CLT-002).
type HandshakeInfo struct {
	// Authenticated is true once the server accepted the credentials (AUTH
	// succeeded or the HELLO reply said so).
	Authenticated bool
	// Capabilities are the capability names from the HELLO reply
	// (HELLO_MANDATORY).
	Capabilities []string
}
