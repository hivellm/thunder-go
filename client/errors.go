package client

import "strings"

// Typed client errors (CLT-050..052). Result::Err(string) replies are parsed
// per the config's error_codes convention (PRO-014) by FromServerMessage into
// a typed error carrying the raw message, an optional machine-readable code
// (from a leading "[code] " prefix), and a stable error class. Applications
// branch on the class and code, never on message text (CLT-052).

// AuthError is an authentication / authorization failure — handshake
// rejections (CLT-003) and NOAUTH/WRONGPASS/NOPERM-prefixed replies (CLT-051).
type AuthError struct{ Msg string }

func (e *AuthError) Error() string { return e.Msg }

// ServerError is the server answering the call with Result::Err (raw message
// plus an optional bracket code).
type ServerError struct {
	Msg  string
	Code string // machine-readable code from a leading "[code] " prefix, or ""
}

func (e *ServerError) Error() string { return e.Msg }

// ConnectionError is a transport-level failure: dial, write, a connection
// dying while a call was pending (CLT-004/030/031), or an invalid endpoint
// (CLT-070).
type ConnectionError struct{ Msg string }

func (e *ConnectionError) Error() string { return e.Msg }

// TimeoutError is the per-call (or connect) timeout elapsing (CLT-020). The
// pending entry was removed; a late response is dropped per CLT-013.
type TimeoutError struct{ Msg string }

func (e *TimeoutError) Error() string {
	if e.Msg == "" {
		return "timed out"
	}
	return e.Msg
}

// FrameTooLargeError is a frame larger than the cap (WIRE-020/021) — raised
// from the length prefix alone, before any body allocation.
type FrameTooLargeError struct {
	Msg   string
	Body  int
	Limit int
}

func (e *FrameTooLargeError) Error() string { return e.Msg }

// DecodeError is a malformed frame body (WIRE-023), or a push frame received
// while push is RESERVED (CLT-060).
type DecodeError struct{ Msg string }

func (e *DecodeError) Error() string { return e.Msg }

func closedError() *ConnectionError { return &ConnectionError{Msg: "client is closed"} }

var authPrefixes = []string{"NOAUTH", "WRONGPASS", "NOPERM"}

// FromServerMessage parses a server error string per the config's convention
// (CLT-050, PRO-014). The returned error's message always carries the raw
// string, verbatim.
//
//   - ErrorResp3Prefixes: NOAUTH/WRONGPASS/NOPERM → AuthError; everything else
//     (ERR ... included) → ServerError.
//   - ErrorBracketCode: a leading "[code] " is extracted into Code; the auth
//     prefixes still map to AuthError regardless of convention (CLT-051).
//   - ErrorBoth: composes the two — bracket code first, then prefixes.
//   - ErrorNone: no parsing; the raw message becomes ServerError.
func FromServerMessage(message string, convention ErrorConvention) error {
	switch convention {
	case ErrorNone:
		return &ServerError{Msg: message}
	case ErrorResp3Prefixes:
		if startsWithAuthPrefix(message) {
			return &AuthError{Msg: message}
		}
		return &ServerError{Msg: message}
	default: // ErrorBracketCode | ErrorBoth
		code, rest := splitBracketCode(message)
		if startsWithAuthPrefix(rest) {
			return &AuthError{Msg: message}
		}
		return &ServerError{Msg: message, Code: code}
	}
}

// startsWithAuthPrefix reports whether the message starts with one of the auth
// prefixes both family conventions use for authentication failures (CLT-051).
func startsWithAuthPrefix(message string) bool {
	for _, prefix := range authPrefixes {
		if strings.HasPrefix(message, prefix) {
			rest := message[len(prefix):]
			if rest == "" || strings.HasPrefix(rest, " ") {
				return true
			}
		}
	}
	return false
}

// splitBracketCode splits a leading "[code] " prefix. The code must be
// non-empty and whitespace-free (machine-readable); anything else leaves the
// message untouched and returns an empty code.
func splitBracketCode(message string) (code, rest string) {
	if !strings.HasPrefix(message, "[") {
		return "", message
	}
	inner := message[1:]
	end := strings.IndexByte(inner, ']')
	if end == -1 {
		return "", message
	}
	c := inner[:end]
	after := inner[end+1:]
	if c != "" && !strings.ContainsAny(c, " \t\n\r\v\f") && strings.HasPrefix(after, " ") {
		return c, after[1:]
	}
	return "", message
}

// asHandshakeError maps server rejections during the handshake to the typed
// auth class (CLT-003); transport failures keep their own class.
func asHandshakeError(err error) error {
	switch e := err.(type) {
	case *AuthError:
		return e
	case *ServerError:
		return &AuthError{Msg: e.Msg}
	default:
		return err
	}
}
