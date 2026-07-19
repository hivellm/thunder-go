package client

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hivellm/thunder-go/wire"
)

// Reconnect backoff: the first re-dial retries after backoffBase, doubling up
// to backoffCap (CLT-030 "capped backoff").
const (
	backoffBase       = 50 * time.Millisecond
	backoffCap        = 500 * time.Millisecond
	reconnectAttempts = 2 // re-dial budget when a call finds the connection dead
	// DefaultClientName is announced in the HELLO map when none is configured.
	DefaultClientName = "thunder-client"
)

// PushHandler receives server-initiated push frames (CLT-060). It runs on the
// reader goroutine — keep it fast and offload real work to a channel.
type PushHandler func(wire.Value)

// result is one delivered demux outcome: a response or a typed error.
type result struct {
	resp *wire.Response
	err  error
}

// waiter is one pending call slot; the reader (or the poisoner) sends exactly
// once on the buffered channel.
type waiter struct {
	ch chan result
}

// writeFailed marks a request that never reached the wire — safe to resend on
// a fresh connection (not a replay; CLT-031 concerns frames that were sent).
type writeFailed struct{ err error }

func (w *writeFailed) Error() string { return w.err.Error() }
func (w *writeFailed) Unwrap() error { return w.err }

// conn is one live connection: socket + demux state + the reader goroutine.
type conn struct {
	netConn net.Conn
	mu      sync.Mutex
	pending map[uint32]*waiter
	writeMu sync.Mutex
	alive   bool
}

func newConn(nc net.Conn) *conn {
	return &conn{netConn: nc, pending: make(map[uint32]*waiter), alive: true}
}

func (cn *conn) isAlive() bool {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	return cn.alive
}

// poison marks the connection dead and fails every pending call with the same
// typed error (CLT-014). Idempotent.
func (cn *conn) poison(err error) {
	cn.mu.Lock()
	cn.alive = false
	drained := cn.pending
	cn.pending = make(map[uint32]*waiter)
	cn.mu.Unlock()
	for _, w := range drained {
		w.ch <- result{err: err}
	}
}

// kill tears the connection down: fail all pending calls typed and close the
// socket (the reader unblocks and exits). Safe from any goroutine.
func (cn *conn) kill(err error) {
	cn.poison(err)
	_ = cn.netConn.Close()
}

// Client is a multiplexed, config-driven Thunder RPC client (SPEC-003).
// Safe for concurrent use: calls may run from any number of goroutines.
type Client struct {
	endpoint     Endpoint
	config       Config
	clientConfig ClientConfig

	idMu   sync.Mutex
	nextID uint32

	gate *gate

	connMu sync.Mutex
	conn   *conn

	reconnectMu sync.Mutex
	closed      atomic.Bool

	pushMu      sync.Mutex
	pushHandler PushHandler

	hsMu          sync.Mutex
	handshakeInfo HandshakeInfo

	unknownDrops atomic.Int64
}

// Connect dials endpoint and runs the configured handshake (CLT-001/002).
// endpoint accepts scheme://host[:port] (the application's own scheme) or bare
// host:port (CLT-070). config is the application's protocol config; a nil
// clientConfig uses DefaultClientConfig.
func Connect(endpoint string, config Config, clientConfig *ClientConfig) (*Client, error) {
	ep, err := ParseEndpoint(endpoint, config)
	if err != nil {
		return nil, err
	}
	cc := DefaultClientConfig()
	if clientConfig != nil {
		cc = *clientConfig
	}
	c := &Client{
		endpoint:     ep,
		config:       config,
		clientConfig: cc,
		nextID:       1,
		gate:         newGate(config.MaxInFlight),
	}
	cn, err := c.establish()
	if err != nil {
		return nil, err
	}
	c.connMu.Lock()
	c.conn = cn
	c.connMu.Unlock()
	return c, nil
}

// -- public API --------------------------------------------------------------

// Call issues one call with the client's default call timeout (CLT-020).
// Concurrent callers multiplex over the one connection; completion order
// follows the server, not submission order (CLT-010).
func (c *Client) Call(ctx context.Context, command string, args ...wire.Value) (wire.Value, error) {
	return c.CallWithTimeout(ctx, c.clientConfig.CallTimeout, command, args...)
}

// CallWithTimeout issues one call bounded by timeout (0 disables the per-call
// deadline, leaving only ctx). Cancelling ctx cancels the call.
func (c *Client) CallWithTimeout(ctx context.Context, timeout time.Duration, command string, args ...wire.Value) (wire.Value, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// CLT-012: bounded in-flight — excess calls wait here, never refused.
	if err := c.gate.acquire(ctx); err != nil {
		return wire.Value{}, err
	}
	defer c.gate.release()

	callCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	budget := reconnectAttempts
	for {
		cn, err := c.liveConn(&budget)
		if err != nil {
			return wire.Value{}, err
		}
		value, err := c.dispatchOnce(callCtx, cn, command, args)
		var wf *writeFailed
		if errors.As(err, &wf) {
			if budget == 0 {
				return wire.Value{}, wf.err
			}
			// The frame never hit the wire: reconnect and resend.
			continue
		}
		return value, err
	}
}

// OnPush registers the push hook (CLT-060). Frames with id == PushID are
// routed here under PushEnabled and never matched against pending calls. The
// handler runs on the reader goroutine — keep it fast.
func (c *Client) OnPush(handler PushHandler) {
	c.pushMu.Lock()
	c.pushHandler = handler
	c.pushMu.Unlock()
}

// Close is an explicit, idempotent close (CLT-004): it fails all in-flight
// calls with a typed connection-closed error and shuts the socket down.
func (c *Client) Close() {
	c.closed.Store(true)
	c.gate.close()
	c.connMu.Lock()
	cn := c.conn
	c.conn = nil
	c.connMu.Unlock()
	if cn != nil {
		cn.kill(closedError())
	}
}

// IsAuthenticated reports whether the current connection's handshake
// authenticated (CLT-003 — auth is sticky per connection).
func (c *Client) IsAuthenticated() bool {
	c.hsMu.Lock()
	defer c.hsMu.Unlock()
	return c.handshakeInfo.Authenticated
}

// IsAlive reports whether the current connection is live — not poisoned
// (CLT-014) and not closed (CLT-004).
func (c *Client) IsAlive() bool {
	if c.closed.Load() {
		return false
	}
	c.connMu.Lock()
	cn := c.conn
	c.connMu.Unlock()
	return cn != nil && cn.isAlive()
}

// Capabilities returns the capabilities the server advertised in the HELLO
// reply.
func (c *Client) Capabilities() []string {
	c.hsMu.Lock()
	defer c.hsMu.Unlock()
	return c.handshakeInfo.Capabilities
}

// HandshakeInfo returns a snapshot of what the handshake learned (CLT-002).
func (c *Client) HandshakeInfo() HandshakeInfo {
	c.hsMu.Lock()
	defer c.hsMu.Unlock()
	return c.handshakeInfo
}

// UnknownResponseDrops reports how many responses matched no pending call and
// were dropped (CLT-013 — client stats, never fatal).
func (c *Client) UnknownResponseDrops() int64 {
	return c.unknownDrops.Load()
}

// Config returns the protocol config this client drives its behavior from.
func (c *Client) Config() Config { return c.config }

// -- internals ---------------------------------------------------------------

// allocID allocates the next request id, skipping PushID (CLT-010).
func (c *Client) allocID() uint32 {
	c.idMu.Lock()
	defer c.idMu.Unlock()
	for {
		id := c.nextID
		c.nextID++ // wraps naturally at 2^32
		if id != wire.PushID {
			return id
		}
	}
}

func (c *Client) setHandshakeInfo(info HandshakeInfo) {
	c.hsMu.Lock()
	c.handshakeInfo = info
	c.hsMu.Unlock()
}

func (c *Client) getPushHandler() PushHandler {
	c.pushMu.Lock()
	defer c.pushMu.Unlock()
	return c.pushHandler
}

// liveConn returns the current live connection, lazily reconnecting when it is
// dead or absent: up to *budget re-dial + re-handshake attempts with capped
// backoff (CLT-030). Never replays in-flight calls — those already failed
// typed when the connection died (CLT-031).
func (c *Client) liveConn(budget *int) (*conn, error) {
	if c.closed.Load() {
		return nil, closedError()
	}
	c.connMu.Lock()
	cn := c.conn
	c.connMu.Unlock()
	if cn != nil && cn.isAlive() {
		return cn, nil
	}
	c.reconnectMu.Lock()
	defer c.reconnectMu.Unlock()
	if c.closed.Load() {
		return nil, closedError()
	}
	// Another caller may have reconnected while we waited.
	c.connMu.Lock()
	cn = c.conn
	c.connMu.Unlock()
	if cn != nil && cn.isAlive() {
		return cn, nil
	}
	var lastErr error = &ConnectionError{Msg: "connection is dead"}
	backoff := backoffBase
	for *budget > 0 {
		*budget--
		newCn, err := c.establish()
		if err != nil {
			var ae *AuthError
			if errors.As(err, &ae) {
				// An auth rejection is deterministic — retrying cannot fix it.
				return nil, err
			}
			lastErr = err
			if *budget > 0 {
				time.Sleep(backoff)
				backoff *= 2
				if backoff > backoffCap {
					backoff = backoffCap
				}
			}
			continue
		}
		c.connMu.Lock()
		c.conn = newCn
		c.connMu.Unlock()
		return newCn, nil
	}
	return nil, lastErr
}

// establish dials (with the connect timeout, TCP_NODELAY on — CLT-001), starts
// the reader goroutine, and runs the configured handshake (CLT-002).
func (c *Client) establish() (*conn, error) {
	addr := net.JoinHostPort(c.endpoint.Host, strconv.Itoa(c.endpoint.Port))
	netConn, err := net.DialTimeout("tcp", addr, c.clientConfig.ConnectTimeout)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, &TimeoutError{Msg: "timed out"}
		}
		return nil, &ConnectionError{Msg: fmt.Sprintf("connect to %s failed: %v", addr, err)}
	}
	if tcp, ok := netConn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	cn := newConn(netConn)
	go c.readerLoop(cn)
	info, err := c.runHandshake(cn)
	if err != nil {
		// Failure tears the connection down; the caller sees the typed error.
		cn.kill(&ConnectionError{Msg: "handshake failed"})
		return nil, err
	}
	c.setHandshakeInfo(info)
	return cn, nil
}

// dispatchOnce runs one request/response attempt on one connection: register
// the pending entry, write the frame (serialized, CLT-011), await the demuxed
// response under the context deadline (CLT-020).
func (c *Client) dispatchOnce(ctx context.Context, cn *conn, command string, args []wire.Value) (wire.Value, error) {
	id := c.allocID()
	req := wire.Request{ID: id, Command: command, Args: args}
	frame, err := wire.EncodeFrame(req)
	if err != nil {
		return wire.Value{}, &ConnectionError{Msg: "encode request: " + err.Error()}
	}
	w := &waiter{ch: make(chan result, 1)}

	cn.mu.Lock()
	// Register while checking liveness in the same critical section the
	// poisoner drains under — a dying connection either fails this entry or is
	// seen dead here.
	if !cn.alive {
		cn.mu.Unlock()
		return wire.Value{}, &writeFailed{err: &ConnectionError{Msg: "connection is dead"}}
	}
	cn.pending[id] = w
	cn.mu.Unlock()

	cn.writeMu.Lock()
	_, werr := cn.netConn.Write(frame)
	cn.writeMu.Unlock()
	if werr != nil {
		cn.mu.Lock()
		delete(cn.pending, id)
		cn.mu.Unlock()
		e := &ConnectionError{Msg: "write failed: " + werr.Error()}
		cn.kill(e)
		return wire.Value{}, &writeFailed{err: e}
	}

	select {
	case res := <-w.ch:
		if res.err != nil {
			return wire.Value{}, res.err
		}
		resp := res.resp
		if resp.IsErr {
			return wire.Value{}, FromServerMessage(resp.Err, c.config.ErrorCodes)
		}
		return resp.OK, nil
	case <-ctx.Done():
		// CLT-020: remove the pending entry on timeout; a late response to this
		// id is dropped per CLT-013.
		cn.mu.Lock()
		delete(cn.pending, id)
		cn.mu.Unlock()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return wire.Value{}, &TimeoutError{Msg: "timed out"}
		}
		return wire.Value{}, &ConnectionError{Msg: "call cancelled: " + ctx.Err().Error()}
	}
}

// readerLoop is the background reader (CLT-010): it reads frames with the
// config's cap, demuxes by id, routes push frames (CLT-060), drops unknown ids
// (CLT-013), and poisons the connection on any failure (CLT-014).
func (c *Client) readerLoop(cn *conn) {
	cap := c.config.MaxFrameBytes
	header := make([]byte, 4)
	var loopErr error
	for {
		if _, err := io.ReadFull(cn.netConn, header); err != nil {
			loopErr = &ConnectionError{Msg: "connection lost: " + err.Error()}
			break
		}
		length := int(binary.LittleEndian.Uint32(header))
		if length > cap {
			// WIRE-020/021: refuse from the prefix alone, before the body is
			// read or allocated.
			loopErr = &FrameTooLargeError{
				Msg:  fmt.Sprintf("frame body %d bytes exceeds limit %d bytes", length, cap),
				Body: length, Limit: cap,
			}
			break
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(cn.netConn, body); err != nil {
			loopErr = &ConnectionError{Msg: "connection lost: " + err.Error()}
			break
		}
		resp, err := wire.DecodeResponseBody(body)
		if err != nil {
			loopErr = &DecodeError{Msg: err.Error()}
			break
		}
		if resp.ID == wire.PushID {
			if c.config.Push == PushEnabled {
				if !resp.IsErr {
					if h := c.getPushHandler(); h != nil {
						safePushCall(h, resp.OK)
					}
				}
				continue
			}
			// Protocol error when push is RESERVED: poison per CLT-014/060.
			loopErr = &DecodeError{Msg: "server sent a push frame but the config reserves PUSH_ID (CLT-060)"}
			break
		}
		cn.mu.Lock()
		w := cn.pending[resp.ID]
		delete(cn.pending, resp.ID)
		cn.mu.Unlock()
		if w != nil {
			r := resp
			w.ch <- result{resp: &r}
		} else {
			// CLT-013: unknown id — count and drop, never fatal.
			c.unknownDrops.Add(1)
		}
	}
	// CLT-014: fail all pending calls typed and close our side.
	cn.kill(loopErr)
}

// safePushCall runs a push hook with a recover guard: a broken hook must not
// take the reader down.
func safePushCall(h PushHandler, v wire.Value) {
	defer func() { _ = recover() }()
	h(v)
}
