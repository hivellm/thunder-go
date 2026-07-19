# Thunder — Go SDK

The Go lane of the Thunder RPC family (`github.com/hivellm/thunder-go`). Wire
bytes are identical to the Rust, TypeScript, Python and C# lanes — every
implementation pins its default test run to `conformance/vectors/*.yaml`
(SPEC-005), so one PR changes wire behavior everywhere or fails CI.

## Layout

- `wire/` — the wire layer (SPEC-001): the 8-variant `Value`, array-encoded
  `Request` / `Response`, `PushID` (= `u32::MAX`), and the length-prefixed
  MessagePack frame codec with the cap checked before body allocation. Encoding
  uses `vmihailenco/msgpack/v5` with `UseCompactInts(true)`, which reproduces
  the reference (rmp-serde) shortest-form integer packing byte-for-byte.
- `client/` — the multiplexed client (SPEC-003) and the protocol `Config`
  (SPEC-002): a reader goroutine demuxes responses by id over channels, with
  per-call `context.Context` cancellation, connect/per-call timeouts, all three
  handshake styles, lazy reconnect with handshake replay, push-hook routing,
  and typed errors classifying both the RESP3-prefix and bracket-code
  conventions.
- `conformance/` — the corpus loader (TST-020), the primary cross-language
  proof, run by the default `go test ./...`.

## Usage

```go
import (
    "context"

    "github.com/hivellm/thunder-go/client"
    "github.com/hivellm/thunder-go/wire"
)

cfg := client.Standard().WithScheme("myapp").WithPort(9000)
cc := client.DefaultClientConfig().WithCredentials(client.TokenCredentials("tok"))
c, err := client.Connect("myapp://localhost:9000", cfg, &cc)
if err != nil { /* ... */ }
defer c.Close()

pong, err := c.Call(context.Background(), "PING")
```

An application starts from `client.Standard()` and overrides only what it
diverges on, in its own repository — Thunder ships one standard and zero
product knowledge (PRO-020/021).

## Test / quality gate

From `go/`:

```
go build ./...
go vet ./...
go test ./...     # includes the conformance corpus (TST-020)
gofmt -l .        # must print nothing
```

## Release train (PKG-050)

Go is the fifth publish lane of the one release train (PKG-011). Unlike the
registry-published lanes (crates.io / npm / PyPI / NuGet), a Go module is
consumed straight from its VCS tag: publishing is `git tag` + `git push`, and
the proxy (`proxy.golang.org`) serves the tagged tree on first `go get`. Because
this module lives in a subdirectory of the `thunder` repo, its module path
carries the repo root and consumers pin the module tag
`go/vX.Y.Z` (the subdirectory-tag convention Go requires for nested modules).
No registry credential and no new CI publish job are needed — the shared
release-train gate (fmt/vet/test on the tagged commit) is the only thing that
must stay green, and this lane adds `gofmt -l`, `go vet`, and `go test ./...`
(corpus included) to it.

## Where this code lives

This module is **mirrored**: it is developed in the
[`hivellm/thunder`](https://github.com/hivellm/thunder) monorepo under `go/`,
and published from [`hivellm/thunder-go`](https://github.com/hivellm/thunder-go)
so the Go module path resolves. In the monorepo the directory is a git
submodule pointing here.

One consequence worth knowing: the conformance tests read the corpus from the
monorepo (`../../conformance/`), which is not present in this standalone
mirror. They **skip** here and **run for real** in the monorepo, where changes
to this code land and where the corpus is the source of truth. A green run in
this repository therefore proves the Go code compiles and its own unit tests
pass — the cross-language byte guarantee is verified upstream.
