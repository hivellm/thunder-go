package client_test

import (
	"testing"

	"github.com/hivellm/thunder-go/client"
)

// TestFromServerMessage pins the error-classification table for both
// conventions, including NOPERM (CLT-050/051, PRO-014).
func TestFromServerMessage(t *testing.T) {
	type want struct {
		kind string // "auth" | "server"
		code string
	}
	cases := []struct {
		name       string
		message    string
		convention client.ErrorConvention
		want       want
	}{
		{"resp3 noauth", "NOAUTH Authentication required.", client.ErrorResp3Prefixes, want{"auth", ""}},
		{"resp3 wrongpass", "WRONGPASS invalid pair", client.ErrorResp3Prefixes, want{"auth", ""}},
		{"resp3 noperm", "NOPERM this user has no permissions", client.ErrorResp3Prefixes, want{"auth", ""}},
		{"resp3 err is server", "ERR unknown command", client.ErrorResp3Prefixes, want{"server", ""}},
		{"bracket code extracted", "[collection_not_found] no such collection", client.ErrorBracketCode, want{"server", "collection_not_found"}},
		{"bracket noperm still auth", "NOPERM nope", client.ErrorBracketCode, want{"auth", ""}},
		{"both noperm auth wins", "NOPERM nope", client.ErrorBoth, want{"auth", ""}},
		{"both bracket + server", "[bad_request] nope", client.ErrorBoth, want{"server", "bad_request"}},
		{"none is server", "NOAUTH x", client.ErrorNone, want{"server", ""}},
		{"noauth without space bare", "NOAUTH", client.ErrorResp3Prefixes, want{"auth", ""}},
		{"noauthx not a prefix", "NOAUTHORIZED bad", client.ErrorResp3Prefixes, want{"server", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := client.FromServerMessage(tc.message, tc.convention)
			switch tc.want.kind {
			case "auth":
				ae, ok := err.(*client.AuthError)
				if !ok {
					t.Fatalf("expected AuthError, got %T (%v)", err, err)
				}
				if ae.Msg != tc.message {
					t.Fatalf("message not verbatim: %q", ae.Msg)
				}
			case "server":
				se, ok := err.(*client.ServerError)
				if !ok {
					t.Fatalf("expected ServerError, got %T (%v)", err, err)
				}
				if se.Msg != tc.message {
					t.Fatalf("message not verbatim: %q", se.Msg)
				}
				if se.Code != tc.want.code {
					t.Fatalf("code: got %q want %q", se.Code, tc.want.code)
				}
			}
		})
	}
}
