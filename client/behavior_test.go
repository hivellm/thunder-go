// Behavioral floor tests for the Thunder client (SPEC-003, CLT-090): in-process
// net loopback responders built on the wire codec stand in for a Thunder server
// (there is no Go server — server is Rust-only, SPEC-004). The client contract
// is exercised end-to-end over real sockets, mirroring rust/thunder/tests/
// behavior.rs in Go idiom.
package client_test

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hivellm/thunder-go/client"
	"github.com/hivellm/thunder-go/wire"
)

const srvCap = 1024 * 1024

// ── profiles (built exactly as an application would; PRO-020) ────────────────

func plainProfile() client.Config {
	return client.Config{
		Scheme:        "test",
		DefaultPort:   0,
		Handshake:     client.HandshakeNone,
		HelloStyle:    client.HelloStyleNotUsed,
		Push:          client.PushReserved,
		MaxFrameBytes: srvCap,
		MaxInFlight:   64,
		ErrorCodes:    client.ErrorNone,
		Tls:           client.TlsOff,
	}
}

func authCommandConfig() client.Config {
	return plainProfile().
		WithHandshake(client.HandshakeAuthCommand).
		WithHelloStyle(client.HelloStyleNotUsed).
		WithErrorCodes(client.ErrorResp3Prefixes)
}

func arglessHelloConfig() client.Config {
	return plainProfile().
		WithHandshake(client.HandshakeAuthCommand).
		WithHelloStyle(client.HelloStyleArgLess).
		WithErrorCodes(client.ErrorResp3Prefixes)
}

func helloMandatoryConfig() client.Config {
	return plainProfile().
		WithHandshake(client.HandshakeHelloMandatory).
		WithHelloStyle(client.HelloStyleMapPayload).
		WithErrorCodes(client.ErrorBracketCode)
}

// ── loopback server helpers ──────────────────────────────────────────────────

func newListener(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln, ln.Addr().String()
}

func readReq(t *testing.T, conn net.Conn) wire.Request {
	t.Helper()
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		t.Fatalf("read header: %v", err)
	}
	length := int(binary.LittleEndian.Uint32(header))
	body := make([]byte, length)
	if _, err := io.ReadFull(conn, body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	req, err := wire.DecodeRequestBody(body)
	if err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return req
}

func writeResp(t *testing.T, conn net.Conn, resp wire.Response) {
	t.Helper()
	frame, err := wire.EncodeFrame(resp)
	if err != nil {
		t.Fatalf("encode response: %v", err)
	}
	if _, err := conn.Write(frame); err != nil {
		t.Fatalf("write response: %v", err)
	}
}

func sendOK(t *testing.T, conn net.Conn, id uint32, v wire.Value) {
	writeResp(t, conn, wire.ResponseOK(id, v))
}

func sendErr(t *testing.T, conn net.Conn, id uint32, msg string) {
	writeResp(t, conn, wire.ResponseErr(id, msg))
}

func connect(t *testing.T, addr string, cfg client.Config, cc *client.ClientConfig) *client.Client {
	t.Helper()
	c, err := client.Connect(addr, cfg, cc)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func mustStr(t *testing.T, v wire.Value) string {
	t.Helper()
	s, ok := v.AsStr()
	if !ok {
		t.Fatalf("expected a str value, got kind %s", v.Kind())
	}
	return s
}

func helloOKReply() wire.Value {
	return wire.Map([]wire.MapEntry{{Key: wire.Str("authenticated"), Val: wire.Bool(true)}})
}

// ── Multiplexing (CLT-010/011) ───────────────────────────────────────────────

func TestPipelinedCallsCompleteInPermutedOrder(t *testing.T) {
	// Five calls answered in an order that is neither submission nor its
	// reverse can only be routed correctly by the id table (CLT-010/011).
	replyOrder := []int{2, 0, 4, 1, 3}
	ln, addr := newListener(t)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, _ := ln.Accept()
		defer conn.Close()
		reqs := make([]wire.Request, 5)
		for i := 0; i < 5; i++ {
			reqs[i] = readReq(t, conn)
		}
		for _, i := range replyOrder {
			sendOK(t, conn, reqs[i].ID, wire.Str(reqs[i].Command))
		}
	}()

	c := connect(t, addr, plainProfile(), nil)
	names := []string{"C1", "C2", "C3", "C4", "C5"}
	results := make([]string, 5)
	var cwg sync.WaitGroup
	for i, name := range names {
		cwg.Add(1)
		go func(i int, name string) {
			defer cwg.Done()
			v, err := c.Call(context.Background(), name)
			if err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			results[i] = mustStr(t, v)
		}(i, name)
	}
	cwg.Wait()
	for i, name := range names {
		if results[i] != name {
			t.Fatalf("call %s resolved with %q", name, results[i])
		}
	}
	wg.Wait()
}

func TestInFlightBoundBackpressures(t *testing.T) {
	ln, addr := newListener(t)
	go func() {
		conn, _ := ln.Accept()
		defer conn.Close()
		// Strictly serial: with MaxInFlight=1 the second call waits for the
		// first permit, never refused (CLT-012).
		for i := 0; i < 2; i++ {
			req := readReq(t, conn)
			sendOK(t, conn, req.ID, wire.Str(req.Command))
		}
	}()

	c := connect(t, addr, plainProfile().WithMaxInFlight(1), nil)
	var wg sync.WaitGroup
	got := make([]string, 2)
	for i, name := range []string{"A", "B"} {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			v, err := c.Call(context.Background(), name)
			if err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			got[i] = mustStr(t, v)
		}(i, name)
	}
	wg.Wait()
	if got[0] != "A" || got[1] != "B" {
		t.Fatalf("got %v", got)
	}
}

func TestStrayResponseIDDropped(t *testing.T) {
	ln, addr := newListener(t)
	go func() {
		conn, _ := ln.Accept()
		defer conn.Close()
		req := readReq(t, conn)
		sendOK(t, conn, 9999, wire.Null()) // nobody asked for this (CLT-013)
		sendOK(t, conn, req.ID, wire.Str("real"))
	}()

	c := connect(t, addr, plainProfile(), nil)
	v, err := c.Call(context.Background(), "GET")
	if err != nil {
		t.Fatal(err)
	}
	if mustStr(t, v) != "real" {
		t.Fatalf("got %q", mustStr(t, v))
	}
	// The stray may still be in flight; poll briefly.
	waitFor(t, func() bool { return c.UnknownResponseDrops() == 1 })
}

// ── Handshakes (CLT-002/003) ─────────────────────────────────────────────────

func TestNoneHandshakeSendsNothing(t *testing.T) {
	ln, addr := newListener(t)
	go func() {
		conn, _ := ln.Accept()
		defer conn.Close()
		req := readReq(t, conn) // first frame must be the user's command
		if req.Command != "PING" {
			t.Errorf("first frame was %q, want PING", req.Command)
		}
		sendOK(t, conn, req.ID, wire.Str("PONG"))
	}()

	c := connect(t, addr, plainProfile(), nil)
	if c.IsAuthenticated() {
		t.Fatal("must not be authenticated under Handshake::None")
	}
	v, err := c.Call(context.Background(), "PING")
	if err != nil || mustStr(t, v) != "PONG" {
		t.Fatalf("v=%v err=%v", v, err)
	}
}

func TestAuthCommandSendsHelloThenAuthAPIKey(t *testing.T) {
	ln, addr := newListener(t)
	go func() {
		conn, _ := ln.Accept()
		defer conn.Close()
		hello := readReq(t, conn)
		if hello.Command != "HELLO" || len(hello.Args) != 0 {
			t.Errorf("HELLO takes no args; got %q args=%d", hello.Command, len(hello.Args))
		}
		sendOK(t, conn, hello.ID, wire.Null())
		auth := readReq(t, conn)
		if auth.Command != "AUTH" || len(auth.Args) != 1 || mustStr(t, auth.Args[0]) != "k-123" {
			t.Errorf("AUTH mismatch: %+v", auth)
		}
		sendOK(t, conn, auth.ID, wire.Str("OK"))
		ping := readReq(t, conn)
		sendOK(t, conn, ping.ID, wire.Str("PONG"))
	}()

	cc := client.DefaultClientConfig().WithCredentials(client.APIKeyCredentials("k-123"))
	c := connect(t, addr, arglessHelloConfig(), &cc)
	if !c.IsAuthenticated() {
		t.Fatal("expected authenticated")
	}
	v, err := c.Call(context.Background(), "PING")
	if err != nil || mustStr(t, v) != "PONG" {
		t.Fatalf("v=%v err=%v", v, err)
	}
}

func TestAuthCommandSendsAuthUserPassNeverHello(t *testing.T) {
	ln, addr := newListener(t)
	go func() {
		conn, _ := ln.Accept()
		defer conn.Close()
		auth := readReq(t, conn) // first frame must be AUTH — no HELLO at all
		if auth.Command != "AUTH" || len(auth.Args) != 2 ||
			mustStr(t, auth.Args[0]) != "root" || mustStr(t, auth.Args[1]) != "hunter2" {
			t.Errorf("AUTH user/pass mismatch: %+v", auth)
		}
		sendOK(t, conn, auth.ID, wire.Str("OK"))
		ping := readReq(t, conn)
		sendOK(t, conn, ping.ID, wire.Str("PONG"))
	}()

	cc := client.DefaultClientConfig().WithCredentials(client.UserPassCredentials("root", "hunter2"))
	c := connect(t, addr, authCommandConfig(), &cc)
	if !c.IsAuthenticated() {
		t.Fatal("expected authenticated")
	}
	if _, err := c.Call(context.Background(), "PING"); err != nil {
		t.Fatal(err)
	}
}

func TestHelloMandatorySendsMapAndExposesCapabilities(t *testing.T) {
	ln, addr := newListener(t)
	go func() {
		conn, _ := ln.Accept()
		defer conn.Close()
		hello := readReq(t, conn)
		if hello.Command != "HELLO" || len(hello.Args) != 1 {
			t.Errorf("HELLO must be first with a map arg: %+v", hello)
			return
		}
		m := hello.Args[0]
		if v, _ := m.MapGet("version"); func() bool { i, ok := v.AsInt(); return !ok || i != 1 }() {
			t.Errorf("version missing/wrong")
		}
		if v, _ := m.MapGet("token"); asStr(v) != "tok-1" {
			t.Errorf("token credential missing")
		}
		if v, _ := m.MapGet("client_name"); asStr(v) != "itest" {
			t.Errorf("client_name missing")
		}
		sendOK(t, conn, hello.ID, wire.Map([]wire.MapEntry{
			{Key: wire.Str("authenticated"), Val: wire.Bool(true)},
			{Key: wire.Str("capabilities"), Val: wire.Array([]wire.Value{wire.Str("search"), wire.Str("insert")})},
		}))
	}()

	cc := client.DefaultClientConfig().
		WithCredentials(client.TokenCredentials("tok-1")).
		WithClientName("itest")
	c := connect(t, addr, helloMandatoryConfig(), &cc)
	if !c.IsAuthenticated() {
		t.Fatal("expected authenticated")
	}
	caps := c.Capabilities()
	if len(caps) != 2 || caps[0] != "search" || caps[1] != "insert" {
		t.Fatalf("capabilities: %v", caps)
	}
}

func TestHandshakeRejectionIsTypedAuthError(t *testing.T) {
	ln, addr := newListener(t)
	go func() {
		conn, _ := ln.Accept()
		defer conn.Close()
		hello := readReq(t, conn)
		sendErr(t, conn, hello.ID, "[unauthorized] invalid api key")
	}()

	cc := client.DefaultClientConfig().WithCredentials(client.APIKeyCredentials("wrong"))
	_, err := client.Connect(addr, helloMandatoryConfig(), &cc)
	var ae *client.AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("expected AuthError, got %T (%v)", err, err)
	}
	if !contains(ae.Msg, "unauthorized") {
		t.Fatalf("message: %s", ae.Msg)
	}
}

// ── Timeouts (CLT-020/001) ───────────────────────────────────────────────────

func TestConnectTimeoutIsTyped(t *testing.T) {
	// TEST-NET-1 (RFC 5737): routable nowhere, so the SYN is dropped and the
	// connect timeout is what ends the dial.
	cc := client.DefaultClientConfig().WithConnectTimeout(150 * time.Millisecond)
	start := time.Now()
	_, err := client.Connect("192.0.2.1:9", plainProfile(), &cc)
	var timeoutErr *client.TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("expected TimeoutError from an unroutable dial, got %T (%v)", err, err)
	}
	if time.Since(start) < 120*time.Millisecond {
		t.Fatalf("dial returned too early (%v) to have honored the timeout", time.Since(start))
	}
}

func TestPerCallTimeoutAndLateResponseDropped(t *testing.T) {
	ln, addr := newListener(t)
	go func() {
		conn, _ := ln.Accept()
		defer conn.Close()
		slow := readReq(t, conn)
		next := readReq(t, conn) // proves the timeout fired client-side
		sendOK(t, conn, slow.ID, wire.Str("late"))
		sendOK(t, conn, next.ID, wire.Str("fresh"))
	}()

	c := connect(t, addr, plainProfile(), nil)
	_, err := c.CallWithTimeout(context.Background(), 100*time.Millisecond, "SLOW")
	var timeoutErr *client.TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("expected TimeoutError, got %T (%v)", err, err)
	}
	v, err := c.Call(context.Background(), "NEXT")
	if err != nil || mustStr(t, v) != "fresh" {
		t.Fatalf("v=%v err=%v", v, err)
	}
	waitFor(t, func() bool { return c.UnknownResponseDrops() == 1 })
}

func TestContextCancellationCancelsCall(t *testing.T) {
	ln, addr := newListener(t)
	go func() {
		conn, _ := ln.Accept()
		defer conn.Close()
		_ = readReq(t, conn) // never answer
		time.Sleep(200 * time.Millisecond)
	}()

	c := connect(t, addr, plainProfile(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := c.CallWithTimeout(ctx, 0, "HANG")
	if err == nil {
		t.Fatal("expected an error from a cancelled call")
	}
}

// ── Reconnection (CLT-030/031) ───────────────────────────────────────────────

func TestReconnectAfterServerDrop(t *testing.T) {
	ln, addr := newListener(t)
	go func() {
		conn, _ := ln.Accept()
		req := readReq(t, conn)
		sendOK(t, conn, req.ID, wire.Str("first"))
		conn.Close() // drop

		conn2, _ := ln.Accept()
		defer conn2.Close()
		req2 := readReq(t, conn2)
		sendOK(t, conn2, req2.ID, wire.Str("second"))
	}()

	c := connect(t, addr, plainProfile(), nil)
	v, err := c.Call(context.Background(), "A")
	if err != nil || mustStr(t, v) != "first" {
		t.Fatalf("v=%v err=%v", v, err)
	}
	waitFor(t, func() bool { return !c.IsAlive() }) // reader observes EOF
	v, err = c.Call(context.Background(), "B")
	if err != nil || mustStr(t, v) != "second" {
		t.Fatalf("v=%v err=%v", v, err)
	}
}

func TestReconnectReplaysHandshake(t *testing.T) {
	ln, addr := newListener(t)
	seen := make(chan string, 4)
	go func() {
		conn, _ := ln.Accept()
		hello := readReq(t, conn)
		sendOK(t, conn, hello.ID, helloOKReply())
		req := readReq(t, conn)
		sendOK(t, conn, req.ID, wire.Str("first"))
		conn.Close() // drop

		conn2, _ := ln.Accept()
		defer conn2.Close()
		for i := 0; i < 2; i++ {
			r := readReq(t, conn2)
			seen <- r.Command
			if r.Command == "HELLO" {
				sendOK(t, conn2, r.ID, helloOKReply())
			} else {
				sendOK(t, conn2, r.ID, wire.Str("second"))
			}
		}
	}()

	cc := client.DefaultClientConfig().WithCredentials(client.APIKeyCredentials("k"))
	c := connect(t, addr, helloMandatoryConfig(), &cc)
	if v, err := c.Call(context.Background(), "A"); err != nil || mustStr(t, v) != "first" {
		t.Fatalf("v=%v err=%v", v, err)
	}
	waitFor(t, func() bool { return !c.IsAlive() })
	if v, err := c.Call(context.Background(), "B"); err != nil || mustStr(t, v) != "second" {
		t.Fatalf("v=%v err=%v", v, err)
	}
	first := <-seen
	second := <-seen
	if first != "HELLO" || second != "B" {
		t.Fatalf("re-dial must replay handshake before the pending call: got %q then %q", first, second)
	}
}

func TestReconnectGivesUpAfterTwoAttempts(t *testing.T) {
	ln, addr := newListener(t)
	var mu sync.Mutex
	accepts := 0
	go func() {
		// Connection 1: serve handshake + one call, then drop.
		conn, _ := ln.Accept()
		mu.Lock()
		accepts++
		mu.Unlock()
		hello := readReq(t, conn)
		sendOK(t, conn, hello.ID, helloOKReply())
		req := readReq(t, conn)
		sendOK(t, conn, req.ID, wire.Str("ok"))
		conn.Close()
		// Re-dial attempts: accept and slam shut before the handshake completes.
		for i := 0; i < 2; i++ {
			c2, _ := ln.Accept()
			mu.Lock()
			accepts++
			mu.Unlock()
			c2.Close()
		}
	}()

	cc := client.DefaultClientConfig().WithCredentials(client.APIKeyCredentials("k"))
	c := connect(t, addr, helloMandatoryConfig(), &cc)
	if _, err := c.Call(context.Background(), "PING"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !c.IsAlive() })
	_, err := c.Call(context.Background(), "PING")
	var ce *client.ConnectionError
	if !errors.As(err, &ce) {
		t.Fatalf("expected ConnectionError after exhausted re-dials, got %T (%v)", err, err)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return accepts == 3 })
}

// ── Error mapping (CLT-050..052) ─────────────────────────────────────────────

func TestResp3ErrorMappingOverTheWire(t *testing.T) {
	ln, addr := newListener(t)
	go func() {
		conn, _ := ln.Accept()
		defer conn.Close()
		for _, msg := range []string{
			"NOAUTH Authentication required.",
			"WRONGPASS invalid username-password pair",
			"NOPERM this user has no permissions",
			"ERR unknown command 'FOO'",
		} {
			req := readReq(t, conn)
			sendErr(t, conn, req.ID, msg)
		}
	}()

	c := connect(t, addr, arglessHelloConfig(), nil)
	for _, cmd := range []string{"GET", "AUTH", "PERM"} {
		_, err := c.Call(context.Background(), cmd)
		var ae *client.AuthError
		if !errors.As(err, &ae) {
			t.Fatalf("%s: expected AuthError, got %T (%v)", cmd, err, err)
		}
	}
	_, err := c.Call(context.Background(), "FOO")
	var se *client.ServerError
	if !errors.As(err, &se) || se.Msg != "ERR unknown command 'FOO'" {
		t.Fatalf("expected ServerError, got %T (%v)", err, err)
	}
}

func TestBracketErrorMappingOverTheWire(t *testing.T) {
	ln, addr := newListener(t)
	go func() {
		conn, _ := ln.Accept()
		defer conn.Close()
		hello := readReq(t, conn)
		sendOK(t, conn, hello.ID, helloOKReply())
		req := readReq(t, conn)
		sendErr(t, conn, req.ID, "[collection_not_found] no such collection: docs")
		req = readReq(t, conn)
		sendErr(t, conn, req.ID, "WRONGPASS invalid username-password pair")
	}()

	cc := client.DefaultClientConfig().WithCredentials(client.APIKeyCredentials("k"))
	c := connect(t, addr, helloMandatoryConfig(), &cc)
	_, err := c.Call(context.Background(), "SEARCH")
	var se *client.ServerError
	if !errors.As(err, &se) || se.Code != "collection_not_found" {
		t.Fatalf("expected ServerError with code, got %T (%v)", err, err)
	}
	// CLT-051 "regardless of convention": the auth prefix still wins even
	// though this config parses bracket codes, not RESP3 prefixes.
	_, err = c.Call(context.Background(), "AUTH")
	var ae *client.AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("expected AuthError, got %T (%v)", err, err)
	}
}

// ── Push frames (CLT-060) ────────────────────────────────────────────────────

func TestPushFramesRouteToHandlerUnderEnabled(t *testing.T) {
	ln, addr := newListener(t)
	go func() {
		conn, _ := ln.Accept()
		defer conn.Close()
		req := readReq(t, conn)
		// A push frame in front of the response: it must reach the handler and
		// never be matched against the pending call.
		writeResp(t, conn, wire.ResponseOK(wire.PushID, wire.Str("evt")))
		sendOK(t, conn, req.ID, wire.Str("PONG"))
	}()

	c := connect(t, addr, plainProfile().WithPush(client.PushEnabled), nil)
	pushed := make(chan string, 1)
	c.OnPush(func(v wire.Value) {
		s, _ := v.AsStr()
		pushed <- s
	})
	v, err := c.Call(context.Background(), "SUBSCRIBE")
	if err != nil || mustStr(t, v) != "PONG" {
		t.Fatalf("v=%v err=%v", v, err)
	}
	select {
	case s := <-pushed:
		if s != "evt" {
			t.Fatalf("push value %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("push handler never fired")
	}
	if c.UnknownResponseDrops() != 0 {
		t.Fatalf("push must not count as an unknown drop")
	}
}

func TestPushUnderReservedPoisonsConnection(t *testing.T) {
	ln, addr := newListener(t)
	go func() {
		conn, _ := ln.Accept()
		_ = readReq(t, conn)
		writeResp(t, conn, wire.ResponseOK(wire.PushID, wire.Null()))
		conn.Close()

		conn2, _ := ln.Accept()
		defer conn2.Close()
		req := readReq(t, conn2)
		sendOK(t, conn2, req.ID, wire.Str("recovered"))
	}()

	c := connect(t, addr, plainProfile(), nil) // push reserved
	_, err := c.Call(context.Background(), "GET")
	var de *client.DecodeError
	if !errors.As(err, &de) {
		t.Fatalf("push under Reserved is a protocol error (CLT-060), got %T (%v)", err, err)
	}
	v, err := c.Call(context.Background(), "GET")
	if err != nil || mustStr(t, v) != "recovered" {
		t.Fatalf("v=%v err=%v", v, err)
	}
}

// ── Poisoning (CLT-014) ──────────────────────────────────────────────────────

func TestOversizedInboundFramePoisons(t *testing.T) {
	ln, addr := newListener(t)
	go func() {
		conn, _ := ln.Accept()
		_ = readReq(t, conn)
		// A length prefix past the profile cap (1000 = 0x3E8, LE) — refused on
		// the prefix alone.
		conn.Write([]byte{0xE8, 0x03, 0x00, 0x00})
		conn.Close()

		conn2, _ := ln.Accept()
		defer conn2.Close()
		req := readReq(t, conn2)
		sendOK(t, conn2, req.ID, wire.Str("recovered"))
	}()

	c := connect(t, addr, plainProfile().WithMaxFrameBytes(64), nil)
	_, err := c.Call(context.Background(), "GET")
	var ftl *client.FrameTooLargeError
	if !errors.As(err, &ftl) {
		t.Fatalf("expected FrameTooLargeError, got %T (%v)", err, err)
	}
	v, err := c.Call(context.Background(), "GET")
	if err != nil || mustStr(t, v) != "recovered" {
		t.Fatalf("v=%v err=%v", v, err)
	}
}

func TestMalformedFramePoisonsWithDecodeError(t *testing.T) {
	ln, addr := newListener(t)
	go func() {
		conn, _ := ln.Accept()
		defer conn.Close()
		_ = readReq(t, conn)
		// Valid length prefix, garbage body (0xc1 is never valid MessagePack).
		conn.Write([]byte{4, 0, 0, 0, 0xc1, 0xc1, 0xc1, 0xc1})
	}()

	c := connect(t, addr, plainProfile(), nil)
	_, err := c.Call(context.Background(), "GET")
	var de *client.DecodeError
	if !errors.As(err, &de) {
		t.Fatalf("expected DecodeError, got %T (%v)", err, err)
	}
}

// ── Lifecycle (CLT-004) ──────────────────────────────────────────────────────

func TestCloseIsIdempotentAndFailsInFlight(t *testing.T) {
	ln, addr := newListener(t)
	go func() {
		conn, _ := ln.Accept()
		defer conn.Close()
		_ = readReq(t, conn) // swallow, never answer
		time.Sleep(500 * time.Millisecond)
	}()

	c := connect(t, addr, plainProfile(), nil)
	done := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), "HANG")
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	c.Close()
	c.Close() // idempotent

	select {
	case err := <-done:
		var ce *client.ConnectionError
		if !errors.As(err, &ce) {
			t.Fatalf("in-flight call must fail with ConnectionError, got %T (%v)", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight call did not fail after close")
	}
	_, err := c.Call(context.Background(), "AFTER")
	var ce *client.ConnectionError
	if !errors.As(err, &ce) {
		t.Fatalf("call after close must fail with ConnectionError, got %T (%v)", err, err)
	}
}

// ── Endpoints (CLT-070) ──────────────────────────────────────────────────────

func TestHTTPURLRejectedAtConnect(t *testing.T) {
	_, err := client.Connect("http://localhost:8080", plainProfile().WithScheme("test"), nil)
	var ce *client.ConnectionError
	if !errors.As(err, &ce) {
		t.Fatalf("expected ConnectionError, got %T (%v)", err, err)
	}
	if !contains(ce.Msg, "RPC-only") || !contains(ce.Msg, "HTTP client") {
		t.Fatalf("message: %s", ce.Msg)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func asStr(v wire.Value) string {
	s, _ := v.AsStr()
	return s
}
