# streamx

**k9s for gRPC and streaming APIs.** A keyboard-driven terminal client that:
- finds services through gRPC reflection or local `.proto` files (no `protoc`)
- shows streams live
- stores requests as plain [OpenCollection](https://github.com/opencollection-dev/opencollection) YAML, the format Bruno uses
- runs the same files unchanged in CI

It covers gRPC (all four call types, TLS/mTLS), WebSocket, Server-Sent Events, and basic HTTP. It ships as one static binary, with no account and no cloud, and works offline.

## Install

```sh
go install github.com/abdussalamfaqih/streamx/cmd/streamx@latest
```

Or download a binary for Linux, macOS or Windows (amd64/arm64) from [Releases](https://github.com/abdussalamfaqih/streamx/releases).

## Quickstart (2 minutes)

This assumes a gRPC server with reflection on at `localhost:50051`. If you don't have one, start the bundled test server from the root of a clone of this repo: `go run ./cmd/testserver`.

**1. Browse and call.**
```sh
mkdir my-api && cd my-api
streamx
```
Type `:connect localhost:50051` and press `enter`. The **Services** tab lists every method. Pick one with `j`/`k` and `enter`, or find one with `/`. You get a new request whose body is a JSON template.

Press `e` to edit the body in `$EDITOR`, then `r` to send. Replies stream in live.
- Client and bidi streams: type JSON messages in the box, `enter` sends, `ctrl+d` ends your side.
- Stream view (`l`): `p` pauses, `/` filters, `c` toggles compact.

**2. Save it.** Press `w`, then `enter`. This writes `<service>/<method>.yml`, e.g. `echo/unary.yml`. The first save also creates `opencollection.yml` and a `.gitignore`, so the folder is an OpenCollection collection you can commit. (Compatibility with the Bruno app is not tested yet.) Quit with `q`.

**3. Run it in CI.** Add an assertion to the file:
```yaml
runtime:
  assertions:
    - expression: res.status        # gRPC code, 0 = OK
      operator: eq
      value: "0"
```
```sh
streamx run . ; echo "exit $?"     # 0 passed · 1 failed · 2 config error
```

## Environments and secrets

```yaml
# environments/local.yml  (commit this)
name: local
dotEnvFilePath: .env          # gitignored; holds the secret values
variables:
  - name: host
    value: localhost:50051
  - name: apiKey
    secret: true              # no value in the file
```
```
# .env  (never committed)
apiKey=sk-live-…
```

- Use `{{host}}` and `{{apiKey}}` anywhere in a request, then select the environment with `--env local`.
- Secret values can also come from OS environment variables, which suits CI. `{{process.env.NAME}}` reads one directly.
- Files only ever hold the `{{placeholders}}`. Secrets are masked as `****` in all output, `-v` logs and transcripts. So are captured values of 8 or more characters, such as tokens; shorter captures, such as ids, are shown.

**Chaining:** a `set-variable` action copies a value from one response into a variable for the next request. The usual case is a token from HTTP passed to gRPC metadata. See `testdata/collection/chain/`.

## CI

```sh
streamx run requests/ --env ci --var host=$HOST -v --transcript run.jsonl
```

- `--var` sets plain, non-secret variables.
- `-v` prints every event.
- `--transcript` writes them as JSON lines.
- Requests run in order: folder, then `info.seq`, then file name.
- Assertions use Bruno's operators (`eq neq gt gte lt lte contains exists isString isNumber isBool`) on:
  - `res.status`
  - `res.headers.x`
  - `res.body.path` (the last message)
  - `res.messages.length`
  - `res.messages[*].path` (every message must pass)
- For streams, set limits so they end. `extensions.streamx.stream.maxMessages` / `timeout` count as a clean stop. Without them, the default 30s timeout counts as a failure.

The full file format, including where streamx needs `extensions.streamx` beyond the OpenCollection spec, is in [docs/format.md](docs/format.md).

## TUI keys

| Where | Keys |
|---|---|
| Anywhere | `r` send · `x` cancel · `tab` Collection/Services · `:` command · `w` save · `s` save stream transcript (JSONL) · `e` edit body in `$EDITOR` · `E` edit whole file · `?` help · `q` quit |
| Sidebar | `j`/`k`/`g`/`G` move · `enter` open · `/` fuzzy find · `l` to stream |
| Stream | `j`/`k` scroll · `ctrl+u`/`ctrl+d` page · `G` follow · `p` pause · `c` compact · `/` filter · `h` to sidebar |
| Stream input (gRPC client/bidi, WebSocket) | `enter` send (JSON for gRPC, any text for WebSocket) · `ctrl+d` end your side · `esc` leave · `i` back in |

**Commands:**
- `:connect host:port` browses any server
- `:set url <url>`
- `:header <name> [value]` (no value removes it)
- `:env <name>`
- `:transcript <file>`
- `:w [path]`, `:q`, `:q!`

Picking a method in **Services** copies metadata and TLS settings from the request you were on, so auth carries over. `w` never overwrites an existing file. Unsaved changes are never dropped without asking.

## gRPC notes

- **No reflection?** Set `grpc.protoFilePath` in the request and `config.protobuf.importPaths` in `opencollection.yml`. Protos are compiled at runtime, so you don't need `protoc`.
- **Reflection behind auth:** request metadata is also sent on reflection calls.
- **TLS:** `grpcs://` turns it on. For a custom CA, set `extensions.streamx.tls.caFile`; `insecure: true` skips verification. For mTLS, use `config.clientCertificates`, matched by host.
- **JSON mapping:** protojson, as in grpcurl. int64 values are strings, bytes are base64, `Any` uses `@type`, and enums are names. Errors name the unknown field and list the valid ones.
- **Error status details** (`google.rpc.BadRequest` and others) are shown with the status.
- **Message size:** up to 64 MB per message.

## Development

```sh
go test -race ./...
go run github.com/fzipp/gocyclo/cmd/gocyclo@v0.6.0 -over 9 -ignore _test .   # CI keeps every function under 10
go run ./cmd/testserver        # gRPC Echo + Types (reflection) on :50051, HTTP/SSE/WebSocket on :8080
go run ./cmd/streamx run testdata/collection/chain testdata/collection/grpc --env local
```

To release, push a `v*` tag. GitHub Actions runs goreleaser (`.goreleaser.yaml`), which builds static binaries for six platforms and the checksums.

MIT licensed.
