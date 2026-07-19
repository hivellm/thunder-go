package wire

import (
	"encoding/binary"
	"fmt"
)

// Length-prefixed MessagePack frame codec (SPEC-001):
//
//	+-------------------+--------------------------+
//	|  length: u32 (LE) |  body: MessagePack bytes |
//	+-------------------+--------------------------+
//	    4 bytes              length bytes
//
// The cap is validated against the length prefix before the body buffer is
// touched (WIRE-020/021), so a hostile prefix cannot exhaust memory. Decoders
// signal partial input by returning a nil message ("need more bytes") and
// consume exactly one frame per decode (WIRE-022).

// FrameTooLargeError is raised when a length prefix declares a body larger
// than the cap (WIRE-020/021) — from the prefix alone, before any body
// allocation.
type FrameTooLargeError struct {
	Body  int
	Limit int
}

func (e *FrameTooLargeError) Error() string {
	return fmt.Sprintf("frame body %d bytes exceeds limit %d bytes", e.Body, e.Limit)
}

// DecodeError is a well-framed frame whose MessagePack body is malformed
// (WIRE-023), or a structural violation of the Request/Response shape.
type DecodeError struct {
	Msg string
}

func (e *DecodeError) Error() string { return e.Msg }

func decodeErr(format string, args ...any) *DecodeError {
	return &DecodeError{Msg: fmt.Sprintf(format, args...)}
}

func frameTooLarge(body, limit int) *FrameTooLargeError {
	return &FrameTooLargeError{Body: body, Limit: limit}
}

// EncodeFrame encodes a message (Request or Response) into one complete frame
// (u32 LE length + body).
func EncodeFrame(msg any) ([]byte, error) {
	body, err := EncodeBody(msg)
	if err != nil {
		return nil, err
	}
	frame := make([]byte, 4+len(body))
	binary.LittleEndian.PutUint32(frame[:4], uint32(len(body)))
	copy(frame[4:], body)
	return frame, nil
}

// TrySplitFrame splits one frame off buf, returning (body, bytesConsumed,
// ok). ok is false with a nil error when the buffer does not yet hold a
// complete frame (read more and retry — WIRE-022). The cap is validated
// against the length prefix before the body is touched (WIRE-020/021); an
// over-cap prefix returns a *FrameTooLargeError.
func TrySplitFrame(buf []byte, max int) (body []byte, consumed int, ok bool, err error) {
	if len(buf) < 4 {
		return nil, 0, false, nil
	}
	length := int(binary.LittleEndian.Uint32(buf[:4]))
	if length > max {
		return nil, 0, false, frameTooLarge(length, max)
	}
	total := 4 + length
	if len(buf) < total {
		return nil, 0, false, nil
	}
	return buf[4:total], total, true, nil
}

// DecodeRequest decodes one Request frame from buf under the cap max. A nil
// *Request with a nil error means "need more bytes" (WIRE-022).
func DecodeRequest(buf []byte, max int) (*Request, int, error) {
	body, consumed, ok, err := TrySplitFrame(buf, max)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, 0, nil
	}
	req, err := DecodeRequestBody(body)
	if err != nil {
		return nil, 0, err
	}
	return &req, consumed, nil
}

// DecodeResponse decodes one Response frame from buf under the cap max. A nil
// *Response with a nil error means "need more bytes" (WIRE-022).
func DecodeResponse(buf []byte, max int) (*Response, int, error) {
	body, consumed, ok, err := TrySplitFrame(buf, max)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, 0, nil
	}
	resp, err := DecodeResponseBody(body)
	if err != nil {
		return nil, 0, err
	}
	return &resp, consumed, nil
}
