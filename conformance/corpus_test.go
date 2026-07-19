// Package conformance_test is the conformance-corpus loader (TST-020): it
// walks ../../conformance/vectors and asserts every vector per its mode. It
// runs in the default `go test ./...` command — never build-tagged, never
// skipped (NFR-03). This is the primary proof that the Go wire layer emits
// and accepts the same bytes as the Rust/TypeScript/Python/C# lanes.
//
// Mode semantics (TST-002, conformance/README.md):
//   - bidirectional — encode(decoded) == frame byte-exact AND
//     decode(frame) == decoded structurally (floats by bit pattern).
//   - decode-only   — decode succeeds and equals decoded; the canonical
//     encoding of decoded must NOT reproduce these legacy bytes.
//   - stream        — frames decode back-to-back, one per decode, consuming
//     the buffer exactly.
//   - incomplete    — decoder asks for more bytes (no value, no error).
//   - reject        — decode fails with the named error class.
//
// max_frame_bytes (optional) overrides the 64 MiB default cap.
package conformance_test

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hivellm/thunder-go/wire"
	"gopkg.in/yaml.v3"
)

type vector struct {
	Name         string    `yaml:"name"`
	Group        string    `yaml:"group"`
	Mode         string    `yaml:"mode"`
	FrameHex     string    `yaml:"frame_hex"`
	Decoded      *decoded  `yaml:"decoded"`
	Frames       []decoded `yaml:"frames"`
	Error        string    `yaml:"error"`
	MaxFrameByte *int      `yaml:"max_frame_bytes"`
	Notes        string    `yaml:"notes"`
}

type decoded struct {
	Kind    string  `yaml:"kind"`
	ID      uint32  `yaml:"id"`
	Command string  `yaml:"command"`
	Args    []node  `yaml:"args"`
	OK      *node   `yaml:"ok"`
	Err     *string `yaml:"err"`
}

// node is one `decoded` value node: {type, value} plus an optional `bits`
// field for floats — the u64 IEEE-754 bit pattern in hex, required for NaN
// and -0.0 where numeric equality cannot pin the wire bytes.
type node struct {
	Type  string    `yaml:"type"`
	Value yaml.Node `yaml:"value"`
	Bits  *string   `yaml:"bits"`
}

func parseHex(t *testing.T, s string) []byte {
	t.Helper()
	fields := strings.Fields(s)
	out := make([]byte, 0, len(fields))
	for _, f := range fields {
		b, err := strconv.ParseUint(f, 16, 8)
		if err != nil {
			t.Fatalf("bad hex byte %q: %v", f, err)
		}
		out = append(out, byte(b))
	}
	return out
}

func nodeToValue(t *testing.T, n node) wire.Value {
	t.Helper()
	switch n.Type {
	case "null":
		return wire.Null()
	case "bool":
		var b bool
		mustDecode(t, &n.Value, &b)
		return wire.Bool(b)
	case "int":
		var i int64
		mustDecode(t, &n.Value, &i)
		return wire.Int(i)
	case "float":
		if n.Bits != nil {
			bits, err := strconv.ParseUint(*n.Bits, 16, 64)
			if err != nil {
				t.Fatalf("bad float bits %q: %v", *n.Bits, err)
			}
			return wire.Float(math.Float64frombits(bits))
		}
		var f float64
		mustDecode(t, &n.Value, &f)
		return wire.Float(f)
	case "str":
		var s string
		mustDecode(t, &n.Value, &s)
		return wire.Str(s)
	case "bytes":
		var s string
		mustDecode(t, &n.Value, &s)
		return wire.Bytes(parseHex(t, s))
	case "array":
		var items []node
		mustDecode(t, &n.Value, &items)
		vs := make([]wire.Value, 0, len(items))
		for _, item := range items {
			vs = append(vs, nodeToValue(t, item))
		}
		return wire.Array(vs)
	case "map":
		// A map value is a list of [key, value] node pairs.
		var pairs []yaml.Node
		mustDecode(t, &n.Value, &pairs)
		entries := make([]wire.MapEntry, 0, len(pairs))
		for _, p := range pairs {
			var kv []node
			if err := p.Decode(&kv); err != nil {
				t.Fatalf("map entry must be a [key, value] pair: %v", err)
			}
			if len(kv) != 2 {
				t.Fatalf("map entry must have 2 elements, got %d", len(kv))
			}
			entries = append(entries, wire.MapEntry{
				Key: nodeToValue(t, kv[0]),
				Val: nodeToValue(t, kv[1]),
			})
		}
		return wire.Map(entries)
	default:
		t.Fatalf("unknown corpus node type: %s", n.Type)
		return wire.Value{}
	}
}

func mustDecode(t *testing.T, n *yaml.Node, out any) {
	t.Helper()
	if err := n.Decode(out); err != nil {
		t.Fatalf("node decode: %v", err)
	}
}

// expected is the frame a `decoded` corresponds to: a Request or a Response.
type expected struct {
	isRequest bool
	req       wire.Request
	resp      wire.Response
}

func toExpected(t *testing.T, d decoded) expected {
	t.Helper()
	switch d.Kind {
	case "request":
		args := make([]wire.Value, 0, len(d.Args))
		for _, a := range d.Args {
			args = append(args, nodeToValue(t, a))
		}
		return expected{isRequest: true, req: wire.Request{ID: d.ID, Command: d.Command, Args: args}}
	case "response":
		if d.OK != nil && d.Err == nil {
			return expected{resp: wire.ResponseOK(d.ID, nodeToValue(t, *d.OK))}
		}
		if d.Err != nil && d.OK == nil {
			return expected{resp: wire.ResponseErr(d.ID, *d.Err)}
		}
		t.Fatalf("response vector needs exactly one of ok/err")
		return expected{}
	default:
		t.Fatalf("unknown decoded kind: %s", d.Kind)
		return expected{}
	}
}

func (e expected) encode(t *testing.T) []byte {
	t.Helper()
	var (
		frame []byte
		err   error
	)
	if e.isRequest {
		frame, err = wire.EncodeFrame(e.req)
	} else {
		frame, err = wire.EncodeFrame(e.resp)
	}
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return frame
}

// assertDecodes decodes one frame from buf under max and asserts it equals e
// structurally. Returns the bytes consumed.
func (e expected) assertDecodes(t *testing.T, buf []byte, max int, name string) int {
	t.Helper()
	if e.isRequest {
		got, consumed, err := wire.DecodeRequest(buf, max)
		if err != nil {
			t.Fatalf("%s: decode request: %v", name, err)
		}
		if got == nil {
			t.Fatalf("%s: request must decode (need more bytes)", name)
		}
		if got.ID != e.req.ID {
			t.Fatalf("%s: id got %d want %d", name, got.ID, e.req.ID)
		}
		if got.Command != e.req.Command {
			t.Fatalf("%s: command got %q want %q", name, got.Command, e.req.Command)
		}
		if len(got.Args) != len(e.req.Args) {
			t.Fatalf("%s: arg count got %d want %d", name, len(got.Args), len(e.req.Args))
		}
		for i := range got.Args {
			if !got.Args[i].Equal(e.req.Args[i]) {
				t.Fatalf("%s: arg[%d] mismatch", name, i)
			}
		}
		return consumed
	}
	got, consumed, err := wire.DecodeResponse(buf, max)
	if err != nil {
		t.Fatalf("%s: decode response: %v", name, err)
	}
	if got == nil {
		t.Fatalf("%s: response must decode (need more bytes)", name)
	}
	if got.ID != e.resp.ID {
		t.Fatalf("%s: id got %d want %d", name, got.ID, e.resp.ID)
	}
	if got.IsErr != e.resp.IsErr {
		t.Fatalf("%s: result arm mismatch (isErr got %v want %v)", name, got.IsErr, e.resp.IsErr)
	}
	if e.resp.IsErr {
		if got.Err != e.resp.Err {
			t.Fatalf("%s: err got %q want %q", name, got.Err, e.resp.Err)
		}
	} else if !got.OK.Equal(e.resp.OK) {
		t.Fatalf("%s: ok value mismatch", name)
	}
	return consumed
}

func vectorDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "conformance", "vectors"))
	if err != nil {
		t.Fatalf("resolve vectors dir: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("conformance/vectors must exist: %v", err)
	}
	return dir
}

func loadVectorPaths(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(vectorDir(t), "*.yaml"))
	if err != nil {
		t.Fatalf("glob vectors: %v", err)
	}
	return paths
}

func TestCorpusVectors(t *testing.T) {
	paths := loadVectorPaths(t)
	checked := 0
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: read: %v", path, err)
		}
		var v vector
		if err := yaml.Unmarshal(raw, &v); err != nil {
			t.Fatalf("%s: does not parse: %v", path, err)
		}
		frame := parseHex(t, v.FrameHex)
		max := wire.DefaultMaxFrameBytes
		if v.MaxFrameByte != nil {
			max = *v.MaxFrameByte
		}
		t.Run(v.Name, func(t *testing.T) {
			runVector(t, v, frame, max)
		})
		checked++
	}
	// Anti-shrink floor (TST-020): the corpus must not silently shrink.
	if checked < 39 {
		t.Fatalf("corpus must not silently shrink (found %d, floor 39)", checked)
	}
}

func runVector(t *testing.T, v vector, frame []byte, max int) {
	switch v.Mode {
	case "bidirectional":
		if v.Decoded == nil {
			t.Fatalf("%s: bidirectional needs decoded", v.Name)
		}
		want := toExpected(t, *v.Decoded)
		// encode(decoded) == frame, byte-exact.
		if got := want.encode(t); !bytesEqual(got, frame) {
			t.Fatalf("%s: encode mismatch\n got: %s\nwant: %s", v.Name, hexOf(got), hexOf(frame))
		}
		// decode(frame) == decoded, structurally (floats by bits).
		consumed := want.assertDecodes(t, frame, max, v.Name)
		if consumed != len(frame) {
			t.Fatalf("%s: consumed %d of %d", v.Name, consumed, len(frame))
		}
	case "decode-only":
		if v.Decoded == nil {
			t.Fatalf("%s: decode-only needs decoded", v.Name)
		}
		want := toExpected(t, *v.Decoded)
		consumed := want.assertDecodes(t, frame, max, v.Name)
		if consumed != len(frame) {
			t.Fatalf("%s: consumed %d of %d", v.Name, consumed, len(frame))
		}
		// Encoding this form is forbidden: the canonical encoding of the same
		// structure must NOT reproduce the legacy bytes (WIRE-011/013).
		if got := want.encode(t); bytesEqual(got, frame) {
			t.Fatalf("%s: legacy form must not be what we emit", v.Name)
		}
	case "stream":
		offset := 0
		for i, d := range v.Frames {
			consumed := toExpected(t, d).assertDecodes(t, frame[offset:], max, v.Name+"["+strconv.Itoa(i)+"]")
			offset += consumed
		}
		if offset != len(frame) {
			t.Fatalf("%s: buffer not fully consumed (%d of %d)", v.Name, offset, len(frame))
		}
	case "incomplete":
		req, _, err := wire.DecodeRequest(frame, max)
		if err != nil {
			t.Fatalf("%s: incomplete input must not error: %v", v.Name, err)
		}
		if req != nil {
			t.Fatalf("%s: must ask for more bytes", v.Name)
		}
	case "reject":
		_, _, err := wire.DecodeRequest(frame, max)
		if err == nil {
			t.Fatalf("%s: must reject", v.Name)
		}
		switch v.Error {
		case "frame_too_large":
			var ftl *wire.FrameTooLargeError
			if !errors.As(err, &ftl) {
				t.Fatalf("%s: expected FrameTooLargeError, got %v", v.Name, err)
			}
		case "decode":
			var de *wire.DecodeError
			if !errors.As(err, &de) {
				t.Fatalf("%s: expected DecodeError, got %v", v.Name, err)
			}
		default:
			t.Fatalf("%s: unknown error class %q", v.Name, v.Error)
		}
	default:
		t.Fatalf("%s: unknown mode %s", v.Name, v.Mode)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hexOf(b []byte) string {
	var sb strings.Builder
	for i, x := range b {
		if i > 0 {
			sb.WriteByte(' ')
		}
		const hexdigits = "0123456789abcdef"
		sb.WriteByte(hexdigits[x>>4])
		sb.WriteByte(hexdigits[x&0xf])
	}
	return sb.String()
}
