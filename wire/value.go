// Package wire is the Thunder wire layer (SPEC-001): the 8-variant Value
// model, the Request/Response frame bodies, and the length-prefixed
// MessagePack frame codec. It is pure — no sockets, no timers, no config
// dependency (WIRE-030) — and its bytes are pinned by conformance/vectors,
// byte-for-byte identical to the Rust, TypeScript, Python and C# lanes.
package wire

import "math"

// WireVersion is the negotiated wire protocol version. v1 is the only
// version anywhere (WIRE-004).
const WireVersion = 1

// PushID is the reserved frame id for server-initiated push frames
// (WIRE-005): u32::MAX. Client demultiplexers route it to the push hook,
// never to a pending call; servers refuse requests carrying it.
const PushID uint32 = 0xFFFF_FFFF

// DefaultMaxFrameBytes is the default frame-body cap: 64 MiB, checked
// against the length prefix before any body allocation (WIRE-020).
const DefaultMaxFrameBytes = 64 * 1024 * 1024

// Kind is one of the eight wire value kinds (WIRE-002).
type Kind uint8

// The 8 wire value kinds (WIRE-002).
const (
	KindNull Kind = iota
	KindBool
	KindInt
	KindFloat
	KindBytes
	KindStr
	KindArray
	KindMap
)

// String renders the kind's lowercase name (matching the corpus node types).
func (k Kind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindBool:
		return "bool"
	case KindInt:
		return "int"
	case KindFloat:
		return "float"
	case KindBytes:
		return "bytes"
	case KindStr:
		return "str"
	case KindArray:
		return "array"
	case KindMap:
		return "map"
	default:
		return "invalid"
	}
}

// MapEntry is one insertion-ordered (key, value) pair of a Map value; keys
// may be any Value (WIRE-002).
type MapEntry struct {
	Key Value
	Val Value
}

// Value is one wire value — byte-compatible with the donor implementations'
// value models (WIRE-002). Construct through the factories (Int, Map, ...);
// Map is an ordered pair list, not a hash map.
type Value struct {
	kind  Kind
	b     bool
	i     int64
	f     float64
	bytes []byte
	s     string
	arr   []Value
	m     []MapEntry
}

// -- factories ---------------------------------------------------------------

// Null is the SQL NULL / nil value.
func Null() Value { return Value{kind: KindNull} }

// Bool builds a boolean value.
func Bool(v bool) Value { return Value{kind: KindBool, b: v} }

// Int builds a signed 64-bit integer value.
func Int(v int64) Value { return Value{kind: KindInt, i: v} }

// Float builds a 64-bit float value (bit pattern preserved on the wire).
func Float(v float64) Value { return Value{kind: KindFloat, f: v} }

// Bytes builds a raw-bytes value. The slice is retained, not copied.
func Bytes(v []byte) Value {
	if v == nil {
		v = []byte{}
	}
	return Value{kind: KindBytes, bytes: v}
}

// Str builds a string value.
func Str(v string) Value { return Value{kind: KindStr, s: v} }

// Array builds an array value from the given items.
func Array(items []Value) Value {
	if items == nil {
		items = []Value{}
	}
	return Value{kind: KindArray, arr: items}
}

// Map builds a map value from the given ordered pairs.
func Map(pairs []MapEntry) Value {
	if pairs == nil {
		pairs = []MapEntry{}
	}
	return Value{kind: KindMap, m: pairs}
}

// -- accessors ---------------------------------------------------------------

// Kind reports the value's variant.
func (v Value) Kind() Kind { return v.kind }

// AsStr extracts the inner string, or ("", false) for other kinds.
func (v Value) AsStr() (string, bool) {
	if v.kind == KindStr {
		return v.s, true
	}
	return "", false
}

// AsBytes extracts bytes (also accepts Str as UTF-8 bytes).
func (v Value) AsBytes() ([]byte, bool) {
	switch v.kind {
	case KindBytes:
		return v.bytes, true
	case KindStr:
		return []byte(v.s), true
	default:
		return nil, false
	}
}

// AsInt extracts an integer, or (0, false).
func (v Value) AsInt() (int64, bool) {
	if v.kind == KindInt {
		return v.i, true
	}
	return 0, false
}

// AsFloat extracts a float (accepts Int widened to float64).
func (v Value) AsFloat() (float64, bool) {
	switch v.kind {
	case KindFloat:
		return v.f, true
	case KindInt:
		return float64(v.i), true
	default:
		return 0, false
	}
}

// AsBool extracts a bool, or (false, false).
func (v Value) AsBool() (bool, bool) {
	if v.kind == KindBool {
		return v.b, true
	}
	return false, false
}

// AsArray extracts the array items, or (nil, false).
func (v Value) AsArray() ([]Value, bool) {
	if v.kind == KindArray {
		return v.arr, true
	}
	return nil, false
}

// AsMap extracts the map pairs, or (nil, false).
func (v Value) AsMap() ([]MapEntry, bool) {
	if v.kind == KindMap {
		return v.m, true
	}
	return nil, false
}

// MapGet looks up a string key in a Map value.
func (v Value) MapGet(key string) (Value, bool) {
	if v.kind != KindMap {
		return Value{}, false
	}
	for _, e := range v.m {
		if s, ok := e.Key.AsStr(); ok && s == key {
			return e.Val, true
		}
	}
	return Value{}, false
}

// IsNull reports whether the value is Null.
func (v Value) IsNull() bool { return v.kind == KindNull }

// Equal reports structural equality, comparing floats by their u64 bit
// pattern so NaN round-trips and -0.0 does not equal 0.0 (WIRE-014).
func (v Value) Equal(o Value) bool {
	if v.kind != o.kind {
		return false
	}
	switch v.kind {
	case KindNull:
		return true
	case KindBool:
		return v.b == o.b
	case KindInt:
		return v.i == o.i
	case KindFloat:
		return math.Float64bits(v.f) == math.Float64bits(o.f)
	case KindStr:
		return v.s == o.s
	case KindBytes:
		if len(v.bytes) != len(o.bytes) {
			return false
		}
		for i := range v.bytes {
			if v.bytes[i] != o.bytes[i] {
				return false
			}
		}
		return true
	case KindArray:
		if len(v.arr) != len(o.arr) {
			return false
		}
		for i := range v.arr {
			if !v.arr[i].Equal(o.arr[i]) {
				return false
			}
		}
		return true
	case KindMap:
		if len(v.m) != len(o.m) {
			return false
		}
		for i := range v.m {
			if !v.m[i].Key.Equal(o.m[i].Key) || !v.m[i].Val.Equal(o.m[i].Val) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// Request is one RPC request (WIRE-001). id is client-chosen and echoed
// back; many requests multiplex over one connection. Serialized as an array
// (WIRE-012); map-shaped requests decode too (WIRE-013).
type Request struct {
	ID      uint32
	Command string
	Args    []Value
}

// Response is one RPC response (WIRE-001). Exactly one of OK / Err is
// meaningful, discriminated by IsErr; v1 carries no structured error object
// — conventions are prefix-based and config-driven (WIRE-040).
type Response struct {
	ID    uint32
	OK    Value
	Err   string
	IsErr bool
}

// ResponseOK builds a success response.
func ResponseOK(id uint32, value Value) Response {
	return Response{ID: id, OK: value}
}

// ResponseErr builds an error response carrying the verbatim message.
func ResponseErr(id uint32, message string) Response {
	return Response{ID: id, Err: message, IsErr: true}
}
