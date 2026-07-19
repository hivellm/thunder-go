package wire

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	var sb strings.Builder
	for i, x := range b {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteByte(digits[x>>4])
		sb.WriteByte(digits[x&0xf])
	}
	return sb.String()
}

// ── Golden vectors (family-pinned bytes, corpus canonical group) ────────────

func TestPingRequestGolden(t *testing.T) {
	req := Request{ID: 1, Command: "PING", Args: nil}
	frame, err := EncodeFrame(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := hexOf(frame); got != "08 00 00 00 93 01 a4 50 49 4e 47 90" {
		t.Fatalf("frame mismatch: %s", got)
	}
	decoded, consumed, err := DecodeRequest(frame, DefaultMaxFrameBytes)
	if err != nil || decoded == nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.ID != 1 || decoded.Command != "PING" || len(decoded.Args) != 0 {
		t.Fatalf("decoded mismatch: %+v", decoded)
	}
	if consumed != len(frame) {
		t.Fatalf("consumed %d of %d", consumed, len(frame))
	}
}

func TestPongResponseNestedOkGolden(t *testing.T) {
	resp := ResponseOK(1, Str("PONG"))
	frame, err := EncodeFrame(resp)
	if err != nil {
		t.Fatal(err)
	}
	if got := hexOf(frame); got != "10 00 00 00 92 01 81 a2 4f 6b 81 a3 53 74 72 a4 50 4f 4e 47" {
		t.Fatalf("frame mismatch: %s", got)
	}
}

func TestNullBareStringAndIntSingleKeyMap(t *testing.T) {
	nb, _ := EncodeBody(ResponseOK(0, Null()))
	// Isolate just the value bytes by encoding a body and checking the tail.
	if got := hexOf(mustEncodeValue(t, Null())); got != "a4 4e 75 6c 6c" {
		t.Fatalf("Null: %s", got)
	}
	_ = nb
	if got := hexOf(mustEncodeValue(t, Int(42))); got != "81 a3 49 6e 74 2a" {
		t.Fatalf("Int(42): %s", got)
	}
}

func TestBytesCanonicalBin(t *testing.T) {
	if got := hexOf(mustEncodeValue(t, Bytes([]byte{1, 2, 3, 255}))); got != "81 a5 42 79 74 65 73 c4 04 01 02 03 ff" {
		t.Fatalf("Bytes: %s", got)
	}
}

// ── Round-trip matrix (WIRE-002/014/015) ────────────────────────────────────

func TestRoundTripAllVariants(t *testing.T) {
	all := Array([]Value{
		Null(), Bool(true), Bool(false),
		Int(0), Int(math.MinInt64), Int(math.MaxInt64), Int(-32), Int(127), Int(255), Int(65535),
		Float(0.0), Float(math.Copysign(0, -1)), Float(math.Inf(1)), Float(math.Inf(-1)),
		Bytes([]byte{}), Bytes([]byte{0, 1, 2, 255}),
		Str(""), Str("héllo wörld"),
		Array([]Value{}), Map([]MapEntry{}),
		Map([]MapEntry{
			{Key: Str("k"), Val: Int(1)},
			{Key: Int(2), Val: Str("non-string key")},
		}),
	})
	resp := ResponseOK(7, all)
	frame, err := EncodeFrame(resp)
	if err != nil {
		t.Fatal(err)
	}
	decoded, consumed, err := DecodeResponse(frame, DefaultMaxFrameBytes)
	if err != nil || decoded == nil {
		t.Fatalf("decode: %v", err)
	}
	if !decoded.OK.Equal(all) {
		t.Fatalf("round-trip mismatch")
	}
	if consumed != len(frame) {
		t.Fatalf("consumed %d of %d", consumed, len(frame))
	}
}

func TestNaNBitPatternSurvives(t *testing.T) {
	frame, _ := EncodeFrame(ResponseOK(1, Float(math.NaN())))
	decoded, _, err := DecodeResponse(frame, DefaultMaxFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := decoded.OK.AsFloat()
	if !ok || math.Float64bits(f) != math.Float64bits(math.NaN()) {
		t.Fatalf("NaN bit pattern not preserved")
	}
}

// ── Framing edges (WIRE-020..023) ───────────────────────────────────────────

func TestPartialHeaderAndBodyNeedMore(t *testing.T) {
	frame, _ := EncodeFrame(Request{ID: 1, Command: "PING"})
	for _, cut := range []int{0, 1, 3, 4, len(frame) - 1} {
		req, _, err := DecodeRequest(frame[:cut], DefaultMaxFrameBytes)
		if err != nil {
			t.Fatalf("cut %d: unexpected error %v", cut, err)
		}
		if req != nil {
			t.Fatalf("cut %d: must ask for more bytes", cut)
		}
	}
}

func TestTwoFramesConsumeOneEach(t *testing.T) {
	a, _ := EncodeFrame(ResponseOK(1, Int(1)))
	b, _ := EncodeFrame(ResponseOK(2, Int(2)))
	buf := append(append([]byte{}, a...), b...)
	first, used, err := DecodeResponse(buf, DefaultMaxFrameBytes)
	if err != nil || first.ID != 1 || used != len(a) {
		t.Fatalf("first frame: %v id=%d used=%d", err, first.ID, used)
	}
	second, used2, err := DecodeResponse(buf[used:], DefaultMaxFrameBytes)
	if err != nil || second.ID != 2 || used2 != len(b) {
		t.Fatalf("second frame: %v", err)
	}
}

func TestOversizedPrefixRejectedBeforeBody(t *testing.T) {
	over := uint32(DefaultMaxFrameBytes + 1)
	buf := []byte{byte(over), byte(over >> 8), byte(over >> 16), byte(over >> 24)}
	_, _, err := DecodeRequest(buf, DefaultMaxFrameBytes)
	var ftl *FrameTooLargeError
	if !errors.As(err, &ftl) {
		t.Fatalf("expected FrameTooLargeError, got %v", err)
	}
	if ftl.Body != DefaultMaxFrameBytes+1 || ftl.Limit != DefaultMaxFrameBytes {
		t.Fatalf("wrong fields: %+v", ftl)
	}
}

func TestGarbageBodyIsDecodeError(t *testing.T) {
	buf := []byte{4, 0, 0, 0, 0xc1, 0xc1, 0xc1, 0xc1}
	_, _, err := DecodeRequest(buf, DefaultMaxFrameBytes)
	var de *DecodeError
	if !errors.As(err, &de) {
		t.Fatalf("expected DecodeError, got %v", err)
	}
}

func TestZeroLengthBodyIsDecodeError(t *testing.T) {
	buf := []byte{0, 0, 0, 0}
	_, _, err := DecodeRequest(buf, DefaultMaxFrameBytes)
	var de *DecodeError
	if !errors.As(err, &de) {
		t.Fatalf("expected DecodeError, got %v", err)
	}
}

func TestPushIDReserved(t *testing.T) {
	if PushID != math.MaxUint32 {
		t.Fatalf("PushID must be u32::MAX, got %d", PushID)
	}
}

func mustEncodeValue(t *testing.T, v Value) []byte {
	t.Helper()
	// Encode a Response OK carrying v, then strip the frame + [id, {"Ok": ...}]
	// prefix to isolate the value's own bytes. Simpler: encode the value via a
	// one-arg request and slice; instead reconstruct through EncodeBody of a
	// response and locate the value tail after the "Ok" key.
	frame, err := EncodeBody(ResponseOK(0, v))
	if err != nil {
		t.Fatal(err)
	}
	// body = 92 00 81 a2 4f 6b <value...>  → strip 6 leading bytes.
	const prefix = 6
	if len(frame) < prefix {
		t.Fatalf("body too short")
	}
	return frame[prefix:]
}
