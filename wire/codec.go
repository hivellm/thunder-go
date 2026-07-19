package wire

import (
	"bytes"
	"fmt"

	"github.com/vmihailenco/msgpack/v5"
	"github.com/vmihailenco/msgpack/v5/msgpcode"
)

// The externally-tagged encoding (rmp-serde default): the unit variant Null
// serializes as the bare string "Null", every payload variant as a single-key
// map ({"Int": 42}), and a Response result nests two one-key maps
// ({"Ok": {"Str": "PONG"}} — WIRE-003, corpus-pinned). Integers pack in the
// shortest form (UseCompactInts); Bytes emit as MessagePack bin (WIRE-010);
// floats always pack as f64 preserving bit patterns (WIRE-014).

// EncodeBody encodes one Request/Response body (no length prefix).
func EncodeBody(msg any) ([]byte, error) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	enc.UseCompactInts(true)
	switch m := msg.(type) {
	case Request:
		if err := encodeRequest(enc, m); err != nil {
			return nil, err
		}
	case *Request:
		if err := encodeRequest(enc, *m); err != nil {
			return nil, err
		}
	case Response:
		if err := encodeResponse(enc, m); err != nil {
			return nil, err
		}
	case *Response:
		if err := encodeResponse(enc, *m); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("EncodeBody needs a Request or Response, got %T", msg)
	}
	return buf.Bytes(), nil
}

func encodeRequest(enc *msgpack.Encoder, r Request) error {
	// [id, command, [args...]] — the array-encoded struct (WIRE-012).
	if err := enc.EncodeArrayLen(3); err != nil {
		return err
	}
	if err := enc.EncodeUint(uint64(r.ID)); err != nil {
		return err
	}
	if err := enc.EncodeString(r.Command); err != nil {
		return err
	}
	if err := enc.EncodeArrayLen(len(r.Args)); err != nil {
		return err
	}
	for i := range r.Args {
		if err := encodeValue(enc, r.Args[i]); err != nil {
			return err
		}
	}
	return nil
}

func encodeResponse(enc *msgpack.Encoder, r Response) error {
	// [id, result] where result is {"Ok": value} or {"Err": string}.
	if err := enc.EncodeArrayLen(2); err != nil {
		return err
	}
	if err := enc.EncodeUint(uint64(r.ID)); err != nil {
		return err
	}
	if err := enc.EncodeMapLen(1); err != nil {
		return err
	}
	if r.IsErr {
		if err := enc.EncodeString("Err"); err != nil {
			return err
		}
		return enc.EncodeString(r.Err)
	}
	if err := enc.EncodeString("Ok"); err != nil {
		return err
	}
	return encodeValue(enc, r.OK)
}

func encodeValue(enc *msgpack.Encoder, v Value) error {
	switch v.kind {
	case KindNull:
		return enc.EncodeString("Null")
	case KindBool:
		if err := tag(enc, "Bool"); err != nil {
			return err
		}
		return enc.EncodeBool(v.b)
	case KindInt:
		if err := tag(enc, "Int"); err != nil {
			return err
		}
		return enc.EncodeInt(v.i)
	case KindFloat:
		if err := tag(enc, "Float"); err != nil {
			return err
		}
		return enc.EncodeFloat64(v.f)
	case KindBytes:
		if err := tag(enc, "Bytes"); err != nil {
			return err
		}
		return enc.EncodeBytes(v.bytes)
	case KindStr:
		if err := tag(enc, "Str"); err != nil {
			return err
		}
		return enc.EncodeString(v.s)
	case KindArray:
		if err := tag(enc, "Array"); err != nil {
			return err
		}
		if err := enc.EncodeArrayLen(len(v.arr)); err != nil {
			return err
		}
		for i := range v.arr {
			if err := encodeValue(enc, v.arr[i]); err != nil {
				return err
			}
		}
		return nil
	case KindMap:
		if err := tag(enc, "Map"); err != nil {
			return err
		}
		if err := enc.EncodeArrayLen(len(v.m)); err != nil {
			return err
		}
		for i := range v.m {
			if err := enc.EncodeArrayLen(2); err != nil {
				return err
			}
			if err := encodeValue(enc, v.m[i].Key); err != nil {
				return err
			}
			if err := encodeValue(enc, v.m[i].Val); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown Value kind: %d", v.kind)
	}
}

// tag writes a fixmap-of-1 header plus the variant name, the opening of an
// externally-tagged payload variant ({"<name>": <payload>}).
func tag(enc *msgpack.Encoder, name string) error {
	if err := enc.EncodeMapLen(1); err != nil {
		return err
	}
	return enc.EncodeString(name)
}

// -- decoding ----------------------------------------------------------------

// DecodeRequestBody decodes one Request body: array-encoded (WIRE-012) or the
// legacy map shape (WIRE-013, decode-only).
func DecodeRequestBody(body []byte) (Request, error) {
	dec := msgpack.NewDecoder(bytes.NewReader(body))
	return decodeRequest(dec)
}

// DecodeResponseBody decodes one Response body. result is the externally-
// tagged {"Ok": value} / {"Err": string} (WIRE-003). Map-shaped responses are
// rejected (no family SDK ever emitted one).
func DecodeResponseBody(body []byte) (Response, error) {
	dec := msgpack.NewDecoder(bytes.NewReader(body))
	return decodeResponse(dec)
}

func decodeRequest(dec *msgpack.Decoder) (Request, error) {
	code, err := dec.PeekCode()
	if err != nil {
		return Request{}, decodeErr("malformed Request body: %v", err)
	}
	switch {
	case isArrayCode(code):
		n, err := dec.DecodeArrayLen()
		if err != nil {
			return Request{}, decodeErr("malformed Request array: %v", err)
		}
		if n != 3 {
			return Request{}, decodeErr("Request array needs 3 elements, got %d", n)
		}
		id, err := decodeFrameID(dec)
		if err != nil {
			return Request{}, err
		}
		command, err := dec.DecodeString()
		if err != nil {
			return Request{}, decodeErr("Request command must be a str: %v", err)
		}
		args, err := decodeValueArray(dec)
		if err != nil {
			return Request{}, err
		}
		return Request{ID: id, Command: command, Args: args}, nil
	case isMapCode(code):
		// WIRE-013: pre-Thunder legacy map-shaped Request.
		return decodeMapShapedRequest(dec)
	default:
		return Request{}, decodeErr("Request body must be an array or map, got code 0x%02x", code)
	}
}

func decodeMapShapedRequest(dec *msgpack.Decoder) (Request, error) {
	n, err := dec.DecodeMapLen()
	if err != nil {
		return Request{}, decodeErr("malformed map-shaped Request: %v", err)
	}
	var (
		req      Request
		haveID   bool
		haveCmd  bool
		haveArgs bool
	)
	for i := 0; i < n; i++ {
		key, err := dec.DecodeString()
		if err != nil {
			return Request{}, decodeErr("map-shaped Request key must be a str: %v", err)
		}
		switch key {
		case "id":
			req.ID, err = decodeFrameID(dec)
			if err != nil {
				return Request{}, err
			}
			haveID = true
		case "command":
			req.Command, err = dec.DecodeString()
			if err != nil {
				return Request{}, decodeErr("map-shaped Request command must be a str: %v", err)
			}
			haveCmd = true
		case "args":
			req.Args, err = decodeValueArray(dec)
			if err != nil {
				return Request{}, err
			}
			haveArgs = true
		default:
			if err := dec.Skip(); err != nil {
				return Request{}, decodeErr("map-shaped Request: %v", err)
			}
		}
	}
	if !haveID || !haveCmd || !haveArgs {
		return Request{}, decodeErr("map-shaped Request is missing a field (id/command/args)")
	}
	return req, nil
}

func decodeResponse(dec *msgpack.Decoder) (Response, error) {
	code, err := dec.PeekCode()
	if err != nil {
		return Response{}, decodeErr("malformed Response body: %v", err)
	}
	if !isArrayCode(code) {
		return Response{}, decodeErr("Response body must be a 2-element array [id, result]")
	}
	n, err := dec.DecodeArrayLen()
	if err != nil {
		return Response{}, decodeErr("malformed Response array: %v", err)
	}
	if n != 2 {
		return Response{}, decodeErr("Response body must be a 2-element array, got %d", n)
	}
	id, err := decodeFrameID(dec)
	if err != nil {
		return Response{}, err
	}
	code, err = dec.PeekCode()
	if err != nil {
		return Response{}, decodeErr("malformed Response result: %v", err)
	}
	if !isMapCode(code) {
		return Response{}, decodeErr("Response result must be a single-key map")
	}
	mlen, err := dec.DecodeMapLen()
	if err != nil {
		return Response{}, decodeErr("malformed Response result: %v", err)
	}
	if mlen != 1 {
		return Response{}, decodeErr("Response result must be {'Ok': ...} or {'Err': ...}")
	}
	arm, err := dec.DecodeString()
	if err != nil {
		return Response{}, decodeErr("Response result arm must be a str: %v", err)
	}
	switch arm {
	case "Ok":
		ok, err := decodeValue(dec)
		if err != nil {
			return Response{}, err
		}
		return Response{ID: id, OK: ok}, nil
	case "Err":
		msg, err := dec.DecodeString()
		if err != nil {
			return Response{}, decodeErr("Err arm must carry a string: %v", err)
		}
		return Response{ID: id, Err: msg, IsErr: true}, nil
	default:
		return Response{}, decodeErr("Response result arm must be Ok or Err, got %q", arm)
	}
}

func decodeValueArray(dec *msgpack.Decoder) ([]Value, error) {
	code, err := dec.PeekCode()
	if err != nil {
		return nil, decodeErr("malformed array: %v", err)
	}
	if !isArrayCode(code) {
		return nil, decodeErr("expected an array, got code 0x%02x", code)
	}
	n, err := dec.DecodeArrayLen()
	if err != nil {
		return nil, decodeErr("malformed array: %v", err)
	}
	out := make([]Value, 0, n)
	for i := 0; i < n; i++ {
		v, err := decodeValue(dec)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func decodeValue(dec *msgpack.Decoder) (Value, error) {
	code, err := dec.PeekCode()
	if err != nil {
		return Value{}, decodeErr("malformed value: %v", err)
	}
	switch {
	case msgpcode.IsString(code):
		s, err := dec.DecodeString()
		if err != nil {
			return Value{}, decodeErr("malformed value string: %v", err)
		}
		if s == "Null" {
			return Null(), nil
		}
		return Value{}, decodeErr("bare string %q is not a Value variant", s)
	case isMapCode(code):
		n, err := dec.DecodeMapLen()
		if err != nil {
			return Value{}, decodeErr("malformed tagged value: %v", err)
		}
		if n != 1 {
			return Value{}, decodeErr("tagged Value must be a single-key map, got %d keys", n)
		}
		tagName, err := dec.DecodeString()
		if err != nil {
			return Value{}, decodeErr("Value tag must be a str: %v", err)
		}
		return decodeTagged(dec, tagName)
	default:
		return Value{}, decodeErr("not a Value: code 0x%02x", code)
	}
}

func decodeTagged(dec *msgpack.Decoder, tagName string) (Value, error) {
	switch tagName {
	case "Bool":
		b, err := dec.DecodeBool()
		if err != nil {
			return Value{}, decodeErr("Bool variant needs a bool payload: %v", err)
		}
		return Bool(b), nil
	case "Int":
		i, err := dec.DecodeInt64()
		if err != nil {
			return Value{}, decodeErr("Int variant needs an i64 payload: %v", err)
		}
		return Int(i), nil
	case "Float":
		// serde-style leniency: an int payload widens to f64.
		code, err := dec.PeekCode()
		if err != nil {
			return Value{}, decodeErr("Float variant: %v", err)
		}
		if isIntCode(code) {
			i, err := dec.DecodeInt64()
			if err != nil {
				return Value{}, decodeErr("Float variant int payload: %v", err)
			}
			return Float(float64(i)), nil
		}
		f, err := dec.DecodeFloat64()
		if err != nil {
			return Value{}, decodeErr("Float variant needs a float payload: %v", err)
		}
		return Float(f), nil
	case "Str":
		s, err := dec.DecodeString()
		if err != nil {
			return Value{}, decodeErr("Str variant needs a str payload: %v", err)
		}
		return Str(s), nil
	case "Bytes":
		return decodeBytesTolerant(dec)
	case "Array":
		items, err := decodeValueArray(dec)
		if err != nil {
			return Value{}, err
		}
		return Array(items), nil
	case "Map":
		return decodeMapValue(dec)
	default:
		return Value{}, decodeErr("unknown Value variant tag %q", tagName)
	}
}

// decodeBytesTolerant reads a Bytes payload as MessagePack bin, or — for the
// pre-Thunder legacy form (WIRE-011) — as an array of byte-range ints.
func decodeBytesTolerant(dec *msgpack.Decoder) (Value, error) {
	code, err := dec.PeekCode()
	if err != nil {
		return Value{}, decodeErr("Bytes variant: %v", err)
	}
	if isArrayCode(code) {
		n, err := dec.DecodeArrayLen()
		if err != nil {
			return Value{}, decodeErr("legacy Bytes int-array: %v", err)
		}
		out := make([]byte, 0, n)
		for i := 0; i < n; i++ {
			u, err := dec.DecodeUint64()
			if err != nil {
				return Value{}, decodeErr("legacy Bytes int-array element: %v", err)
			}
			if u > 255 {
				return Value{}, decodeErr("legacy Bytes int-array holds a non-byte element: %d", u)
			}
			out = append(out, byte(u))
		}
		return Bytes(out), nil
	}
	b, err := dec.DecodeBytes()
	if err != nil {
		return Value{}, decodeErr("Bytes variant needs bin or an int array: %v", err)
	}
	return Bytes(b), nil
}

func decodeMapValue(dec *msgpack.Decoder) (Value, error) {
	code, err := dec.PeekCode()
	if err != nil {
		return Value{}, decodeErr("Map variant: %v", err)
	}
	if !isArrayCode(code) {
		return Value{}, decodeErr("Map variant needs a pair-list payload")
	}
	n, err := dec.DecodeArrayLen()
	if err != nil {
		return Value{}, decodeErr("malformed Map payload: %v", err)
	}
	pairs := make([]MapEntry, 0, n)
	for i := 0; i < n; i++ {
		code, err := dec.PeekCode()
		if err != nil {
			return Value{}, decodeErr("Map entry: %v", err)
		}
		if !isArrayCode(code) {
			return Value{}, decodeErr("Map entry must be a [key, value] pair")
		}
		plen, err := dec.DecodeArrayLen()
		if err != nil {
			return Value{}, decodeErr("malformed Map entry: %v", err)
		}
		if plen != 2 {
			return Value{}, decodeErr("Map entry must be a [key, value] pair, got %d elements", plen)
		}
		key, err := decodeValue(dec)
		if err != nil {
			return Value{}, err
		}
		val, err := decodeValue(dec)
		if err != nil {
			return Value{}, err
		}
		pairs = append(pairs, MapEntry{Key: key, Val: val})
	}
	return Map(pairs), nil
}

func decodeFrameID(dec *msgpack.Decoder) (uint32, error) {
	u, err := dec.DecodeUint64()
	if err != nil {
		return 0, decodeErr("frame id must be a u32: %v", err)
	}
	if u > uint64(^uint32(0)) {
		return 0, decodeErr("frame id must be a u32, got %d", u)
	}
	return uint32(u), nil
}

func isMapCode(c byte) bool {
	return msgpcode.IsFixedMap(c) || c == msgpcode.Map16 || c == msgpcode.Map32
}

func isArrayCode(c byte) bool {
	return msgpcode.IsFixedArray(c) || c == msgpcode.Array16 || c == msgpcode.Array32
}

func isIntCode(c byte) bool {
	if msgpcode.IsFixedNum(c) {
		return true
	}
	switch c {
	case msgpcode.Uint8, msgpcode.Uint16, msgpcode.Uint32, msgpcode.Uint64,
		msgpcode.Int8, msgpcode.Int16, msgpcode.Int32, msgpcode.Int64:
		return true
	default:
		return false
	}
}
