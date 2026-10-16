# freezen

An OpenCode Zen **free** model server, written in Go. It serves the models Zen
gives away through the public `public` credential, in the three request formats
Zen asks its models at: OpenAI Chat Completions, OpenAI Responses and Anthropic
Messages.

```
go run . -c config.yaml
```

| Endpoint | Format |
| --- | --- |
| `POST /v1/chat/completions` | OpenAI Chat Completions (`/chat/completions` works too) |
| `POST /v1/responses` | OpenAI Responses (`/responses`) |
| `POST /v1/messages` | Anthropic Messages (`/messages`) |
| `GET /v1/models` | what Zen serves free now |
| `GET /healthz` | liveness |

The server takes one setting on the command line — `-c`, the path of a YAML
file (`config.yaml` by default; `config.example.yaml` is a copy). A file that
isn't there is not an error: the defaults below are used, so `go run .` serves
on `:8080` with no proxy. Everything else is written down in the source:
`https://opencode.ai/inference/v1` and the two sources the models are read from.

## Config file

```yaml
addr: ":8080"                # where to listen
api_key: "public"            # what every upstream request is signed with
proxy:                       # a pool, empty for a direct connection
  - "http://127.0.0.1:8080"
  - "https://user:pass@proxy.example:8443"
  - "socks5://127.0.0.1:1080"
proxy_cooldown: 60s          # how long a proxy that failed is left out
proxy_required: false        # refuse rather than fall back to a direct connection
log_level: "info"            # debug, info, warn or error
```

`api_key` defaults to `public`, the credential Zen's free tier answers. Each
`proxy` entry is an `http://`, `https://` or `socks5://` URL (a bare
`host:port` is read as `http://`, and a URL may carry credentials). Set
`log_level` to `debug` to see each request and the proxy it goes through.

### The proxy pool

A request through the pool picks one of its proxies **at random**. When a proxy
fails to carry a request (the connection is refused, the handshake or dial
times out, and so on) it is set aside for `proxy_cooldown` — the request is
tried again through another available proxy — and only then offered again. If
every proxy is cooling down, the request falls back to a direct connection
unless `proxy_required` is set. Model discovery goes through the same pool.


## Which models are served

The list is asked again every hour, and whenever a request names a model the
last list didn't have. Three sources, as the plugin asks them:

1. `https://opencode.ai/inference/v1/models` — what Zen serves now. A model it
   no longer gives free leaves the list, one it made free joins it. If this
   can't be read, the last list is kept.
2. `models.opencode.ai/api.json` — each model's context window, output limit,
   modalities, reasoning levels and whether it costs nothing.
3. OpenCode's `models.mdx` — which endpoint each model is asked at.

A model is offered when its id says it is free (`-free`) or the catalog says it
costs nothing, and it isn't retired; a `-free` model neither of the other two
describes yet is offered on Chat Completions, the way OpenCode asks a model with
no package of its own. `GET /v1/models` answers the list, each entry naming the
endpoint it is asked at. A request for a model that isn't free now is a 404
naming the free ones.

## How a turn is sent

- **Only OpenCode 1.19.0's own headers**, so a caller's User-Agent,
  `x-stainless-*` or `x-session-affinity` never marks the request as not
  OpenCode's. Anthropic requests are signed with the `api_key` (default
  `public`) as `x-api-key` and `anthropic-version`, the others with
  `authorization: Bearer <api_key>`.
- **One session per conversation**, keyed by what it first said (or by the
  caller's own session id when it is one OpenCode could have made), carried in
  `x-opencode-session`.
- **The five core tools.** Zen's free tier answers 403 FreeTierError to a
  request without tools, so a request the agent made without `bash`, `edit`,
  `glob`, `grep` or `read` is sent with that tool added, described as *not
  available* and carrying OpenCode's own first parameter.
- **Always streamed upstream**, with `stream_options.include_usage` for Chat
  Completions; a client that didn't ask for a stream gets the reply collapsed
  into one ordinary JSON reply.

## How a reply is handed back

Zen's models call OpenCode's own tool names whatever the agent declared, and
call tools no agent has.

- A call is given the agent's tool whose name differs only in case (`Bash` →
  `bash`), or else only in case and separators (`todowrite` → `todo_write`),
  when exactly one does. A name two of the agent's tools fold to is never
  guessed between.
- A call with no such tool is left out of the reply, and the reply's text says
  which name was left out, so the agent's user and the model's next turn both
  read it. A turn whose every call was left out ends as an ordinary answer
  (`stop` / `end_turn`). Tools run in the agent.
- Chat Completions tool calls are held until the choice finishes, so a name is
  renamed across every fragment of it. In a Responses or Messages reply a call
  left out is streamed as text in its place, so no later index moves and its
  arguments are never sent.

Now and then Zen closes a streamed Chat Completions reply before its `[DONE]`.
When the reply has said something and no tool call of it is unfinished, it ends
there as an ordinary answer and its text says it was cut short. A reply cut
short with a tool call unfinished, or with nothing said, fails with an error
naming the reply's id and how much of it came, so it can be asked again.

## Files

The entry point is `main.go` at the root; everything else lives in `internal`,
split by what it does.

| File | Holds |
| --- | --- |
| `main.go` | the entry point: the config file, wiring, listening |
| `internal/config/config.go` | the YAML config file and its defaults |
| `internal/logging/logger.go` | the leveled log the config file sets |
| `internal/proxy/proxy.go` | the pool a request picks a proxy from |
| `internal/opencode/opencode.go` | OpenCode's version, ids and user-agent |
| `internal/zen/provider.go` | model discovery, sending a request to Zen |
| `internal/zen/models.go` | which models are free, what each is asked at, its limits |
| `internal/zen/headers.go` | the headers Zen is asked with |
| `internal/server/server.go` | routes, a request's way through, handing the reply back |
| `internal/server/request.go` | tool stand-ins, usage, the request body |
| `internal/server/session.go` | one session per conversation |
| `internal/server/guard.go` | renaming and leaving out tool calls |
| `internal/server/stream.go` | reading, guarding and ending an SSE reply |
| `internal/server/collapse.go` | collapsing a stream into one JSON reply |
| `internal/server/types.go` | the wire types of the three formats |
| `internal/server/jsonutil.go` | raw JSON values and small shared helpers |

```
go test ./...
```

The logic follows
[`@magpie-community/opencode-zen-free-auth`](https://github.com/magpie-community/plugins/tree/main/packages/zen-free).
