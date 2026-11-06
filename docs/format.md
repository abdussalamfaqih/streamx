# streamx collection format

streamx reads and writes **OpenCollection YAML**.

Pinned to:
- schema `$id` `https://schema.opencollection.com/opencollection/v1.0.0.json`
- source: `opencollection-dev/opencollection`, package `oc-schema` 0.15.0 (MIT), checked 2026-11-05

The schema sets `additionalProperties: false` on every object. Its only extension point is the collection-root `extensions:` object. streamx adds an `extensions.streamx` block to request files, but only when a request uses a feature OpenCollection lacks. A request without one is plain, schema-valid OpenCollection.

> Open risk: a strict validator (possibly Bruno) may reject or strip `extensions` in request files. Not yet tested against Bruno: installing its CLI was blocked, so open a collection in the Bruno app to check.

## Layout (on disk, `bundled: false`)

```
my-collection/
├── opencollection.yml        # root: opencollection: 1.0.0, info, config, extensions
├── environments/
│   ├── local.yml
│   └── .env                  # secret values, gitignored (via dotEnvFilePath)
└── users/
    ├── folder.yml
    └── get-user.yml          # one request per file
```

## Request type → engine

| `info.type` | Top-level key | streamx engine |
|---|---|---|
| `grpc` | `grpc:` | gRPC |
| `websocket` | `websocket:` | WebSocket. Also accept `ws`, which Bruno's docs use. Saves keep the file's own spelling. |
| `http` | `http:` | HTTP. **SSE** when the response `Content-Type` is `text/event-stream`, so SSE needs no new type. |
| `graphql`, `folder`, `script`, `app` | | Ignored in MVP: listed, not runnable. |

## Field mapping

| streamx `core.Request` | OpenCollection key | Notes |
|---|---|---|
| target | `grpc.url` | `grpcs://` or `https://` turns TLS on. `grpc://` or `http://` means plaintext. A bare `host:port` is plaintext unless a TLS option is set. A missing port defaults to 443 (TLS) or 80. |
| method | `grpc.method` | `pkg.Service/Method`. `/pkg.Service/Method` and grpcurl's `pkg.Service.Method` are also accepted. |
| call type | `grpc.methodType` | `unary` \| `client-streaming` \| `server-streaming` \| `bidi-streaming`. If missing, it is taken from the descriptor. If it disagrees with the descriptor, that is an error. |
| proto source | `grpc.protoFilePath`, root `config.protobuf.importPaths[]` | No `protoFilePath` means server reflection is used. Metadata is sent on reflection calls too. Relative paths (proto, import paths, certs, `caFile`) resolve against the collection root, after `{{var}}` expansion. A proto file under no import path gets its own directory added as one. |
| metadata | `grpc.metadata[] {name, value, disabled}` | |
| single message | `grpc.message` (string) | If it is a list of variants, the one with `selected: true` (else the first) is used. With no message, unary and server-streaming calls send `{}`, like grpcurl. |
| **message list (client/bidi stream)** | `extensions.streamx.messages[]` | **Gap.** OC variants are alternatives, not a send sequence. |
| mTLS client cert | `config.clientCertificates[]` / env `clientCertificates[]` `{domain, type: pem, certificateFilePath, privateKeyFilePath}` | Matched by host. pkcs12 is not supported in the MVP, and using it gives a clear error. |
| **CA cert / insecure** | `extensions.streamx.tls {caFile, insecure}` | **Gap.** |
| **stream limits** | `extensions.streamx.stream {maxMessages, timeout}` | **Gap.** The OC `settings.timeout` (ms) on WebSocket is also read. When `run` stops a stream at either limit, that is a clean end: status 0 / OK. With neither set, `run` uses a 30s timeout, and hitting it is a failure. |
| WS url / headers | `websocket.url`, `websocket.headers[] {name, value, disabled}` | |
| WS message(s) | `websocket.message {type, data}` or variants. List: `extensions.streamx.messages[]` | |
| HTTP | `http.method`, `http.url`, `http.headers[]`, `http.body` | |
| assertions | `runtime.assertions[] {expression, operator, value, disabled}` | Native for http and grpc. **WebSocket gap:** `extensions.streamx.assertions[]` with the same shape. |
| capture | `runtime.actions[] {type: set-variable, phase: after-response, selector: {expression, method: jsonq}, variable: {name, scope: runtime}}` | Native on http only. **gRPC gap:** `extensions.streamx.actions[]` with the same shape. Every `scope` is kept in memory only: captures are never written to files. Captured values of 8 or more characters (tokens, keys) are masked like secrets, even in the response that carried them. Shorter values (ids, flags) are shown, because masking them would garble every matching substring. `selector.method` must be `jsonq` or empty. Expressions use the assertion path syntax below. |
| scripts | `runtime.scripts` | Not run: no scripting (PRD decision 7). The runner warns once per file. |

Extension block example (only present when needed):

```yaml
info:
  name: chat
  type: grpc
grpc:
  url: grpcs://api.local:443
  method: chat.Chat/Talk
  methodType: bidi-streaming
extensions:
  streamx:
    messages:
      - '{"text":"hi"}'
      - '{"text":"bye"}'
    stream:
      maxMessages: 10
      timeout: 5s
    tls:
      caFile: ./certs/ca.pem
```

## Assertion operators

OpenCollection's `operator` field is free-form text. streamx supports these:
`eq neq gt gte lt lte contains exists isString isNumber isBool`

Any other operator is an error (exit code 2), never skipped silently.

Expressions:
- `res.status`: the gRPC code number (0 = OK) or the HTTP status
- `res.statusText`: the gRPC code name (`OK`, `NotFound`, …)
- `res.headers.<name>`: response headers and trailers, lower-cased
- `res.body.<path>`, which uses the last message for streams
- `res.messages.length`
- `res.messages[N].<path>` (`[-1]` is the last message), and `res.messages[*].<path>`, where every message must pass
- `$.<path>` or a bare `<path>`: the same as `res.body.<path>`
- Paths use `.key`, `[N]`, `['key with spaces']` and `.length`. No filters.

Values are parsed as JSON when they can be (`200`, `"x"`, `true`), and otherwise used as plain text. Numeric strings compare as numbers, because protojson writes int64 as strings.

A request **with** assertions passes or fails on its assertions only, so you can assert an expected `NotFound`. A request **without** assertions fails on a non-OK gRPC status or an HTTP status of 400 or more.

## Environments and secrets

- Environment files live in `environments/<name>.yml`, or in `config.environments[]` in the root file. Both are read. A file has `name`, `variables[]`, `extends`, `dotEnvFilePath` and `clientCertificates`.
- **Secrets** are `variables[]` entries with `secret: true` and **no value in the file**. Their values come from:
  1. the environment's `dotEnvFilePath` file (gitignored)
  2. an OS environment variable with the same name, if the file doesn't set it
- `{{process.env.NAME}}` reads an OS environment variable directly. It counts as a secret.
- Secret values are masked as `****` in every output: CLI lines, `-v` events, TUI, and transcripts, including their JSON-escaped form. Captured values are masked from 8 characters up (see capture above).
- `externalSecrets` (Vault, AWS, …) is not supported in the MVP. It gives a clear error.

## Headless run

```
streamx run <path|glob|dir>... [--env NAME] [--var name=value]... [-v] [--transcript FILE]
```

- Directories are walked for request files. Order: directory, then `info.seq`, then file name.
- `--var` values are not secret and override environment variables.
- `-v` prints every event, redacted. Events are printed once the request is done, after its captures ran, so a captured token is masked even in the response that carried it.
- `--transcript FILE` writes every event as one JSON line, `{"request", "time", "kind", "data", "meta", "code", "text", "err"}`, redacted, with file mode 0600. It overwrites the file.
- Exit codes:
  - `0`: all passed
  - `1`: a request or assertion failed, including a request whose `{{var}}` comes from a failed capture
  - `2`: a parse, config or usage error, which stops the run before anything is sent

## Protocol notes

- **HTTP:** `http.body` of type json, text or xml. A matching `Content-Type` is added unless you set one. No scheme in the url means `http://`. Redirects are followed. Bodies over 32 MB fail. A non-JSON body becomes a JSON string, so `res.body` is that string. `http.params` and `http.auth` are ignored with a warning; put them in the url or a header.
- **SSE:** any HTTP response with `Content-Type: text/event-stream`. Each message is one received event, `data` lines are joined with `\n`, and `meta.event` / `meta.id` are set when the server sends them. In `run`, the stream ends when the server closes it or a stream limit hits.
- **WebSocket:** messages are sent as text frames. Received binary frames show as base64 strings. Closing your side (end of `messages` in `run`, `ctrl+d` in the TUI) does not close the socket; it stays open until the server closes it or a limit or cancel stops it. `res.status` is the close code (1000 normal). When streamx stops it, the status is 0.

## Saving

Saves edit the `yaml.Node` tree in place, so key order, comments and unknown keys survive. Whole structs are never re-encoded.
