package client

import (
	"context"
	"errors"

	"github.com/hivellm/thunder-go/wire"
)

// runHandshake runs the configured handshake before user calls proceed
// (CLT-002): HandshakeNone sends nothing; HandshakeAuthCommand sends the
// optional arg-less HELLO (when the application has one) then AUTH when
// credentials are configured; HandshakeHelloMandatory sends the HELLO map as
// the first frame and parses the reply.
//
// Under HandshakeAuthCommand, no credentials means no AUTH frame — the correct
// behavior against a deployment that does not require them. Enforcement is the
// server's policy, not the protocol config's.
func (c *Client) runHandshake(cn *conn) (HandshakeInfo, error) {
	// One round-trip: server rejections surface as the typed auth class, never
	// a generic error (CLT-003).
	call := func(command string, args []wire.Value) (wire.Value, error) {
		ctx := context.Background()
		var cancel context.CancelFunc
		if c.clientConfig.CallTimeout > 0 {
			ctx, cancel = context.WithTimeout(ctx, c.clientConfig.CallTimeout)
			defer cancel()
		}
		v, err := c.dispatchOnce(ctx, cn, command, args)
		if err != nil {
			var wf *writeFailed
			if errors.As(err, &wf) {
				err = wf.err
			}
			return wire.Value{}, asHandshakeError(err)
		}
		return v, nil
	}

	switch c.config.Handshake {
	case HandshakeNone:
		return HandshakeInfo{}, nil

	case HandshakeAuthCommand:
		creds := c.clientConfig.Credentials
		if creds == nil {
			return HandshakeInfo{}, nil
		}
		if c.config.HelloStyle == HelloStyleArgLess {
			// Optional metadata HELLO — takes no arguments; credentials go in
			// the AUTH below.
			if _, err := call("HELLO", nil); err != nil {
				return HandshakeInfo{}, err
			}
		}
		authArgs := make([]wire.Value, 0, len(creds.Secrets))
		for _, s := range creds.Secrets {
			authArgs = append(authArgs, wire.Str(s))
		}
		if _, err := call("AUTH", authArgs); err != nil {
			return HandshakeInfo{}, err
		}
		return HandshakeInfo{Authenticated: true}, nil

	default: // HandshakeHelloMandatory
		// The HELLO map is the first frame (PRO-001). Pair order (version,
		// credential, client_name) is corpus-pinned.
		pairs := []wire.MapEntry{{Key: wire.Str("version"), Val: wire.Int(1)}}
		creds := c.clientConfig.Credentials
		if creds != nil {
			if creds.Kind == CredUserPass {
				return HandshakeInfo{}, &AuthError{Msg: "user/password credentials are not supported under HELLO_MANDATORY - use a token or api_key (PRO-001)"}
			}
			key := "api_key"
			if creds.Kind == CredToken {
				key = "token"
			}
			pairs = append(pairs, wire.MapEntry{Key: wire.Str(key), Val: wire.Str(creds.Secrets[0])})
		}
		name := c.clientConfig.ClientName
		if name == "" {
			name = DefaultClientName
		}
		pairs = append(pairs, wire.MapEntry{Key: wire.Str("client_name"), Val: wire.Str(name)})
		reply, err := call("HELLO", []wire.Value{wire.Map(pairs)})
		if err != nil {
			return HandshakeInfo{}, err
		}
		return parseHelloInfo(reply), nil
	}
}

// parseHelloInfo extracts authenticated / capabilities from a HELLO reply map.
func parseHelloInfo(reply wire.Value) HandshakeInfo {
	info := HandshakeInfo{}
	if node, ok := reply.MapGet("authenticated"); ok {
		if b, ok := node.AsBool(); ok {
			info.Authenticated = b
		}
	}
	if node, ok := reply.MapGet("capabilities"); ok {
		if items, ok := node.AsArray(); ok {
			for _, item := range items {
				if s, ok := item.AsStr(); ok {
					info.Capabilities = append(info.Capabilities, s)
				}
			}
		}
	}
	return info
}
