# Working in this repo

MCP server exposing Melbourne public transport over stdio. The transport data,
planning and database lifecycle all live in
[ptv-gtfs-go](https://github.com/kensantoso/ptv-gtfs-go); this repo is the MCP
layer and nothing else. If a change is about GTFS, timetables or routing, it
almost certainly belongs in the library rather than here.

## Commands

```sh
go build ./... && go vet ./... && go test ./...
gofmt -l .                       # must print nothing; CI fails otherwise

go run ./cmd/ptv-mcp -rebuild            # build the database (~250 MB download)
go run ./cmd/ptv-mcp -modes train -rebuild   # 97 MB instead of 1.5 GB
go run ./cmd/ptv-mcp -index-path         # where it lives
```

Exercising a tool without a client, since the server speaks JSON-RPC on stdin
and closes on EOF — hold the pipe open or you get `server is closing: EOF`:

```sh
{ echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}'
  sleep 1
  echo '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
  sleep 2
} | go run ./cmd/ptv-mcp
```

Packing the Claude Desktop bundle:

```sh
go build -o bundle/server/ptv-mcp ./cmd/ptv-mcp
codesign --force -s - bundle/server/ptv-mcp
npx @anthropic-ai/mcpb pack bundle ptv-mcp.mcpb
```

The `codesign` step is not optional. macOS caches a signature against a path, so
overwriting a binary in place invalidates it and the process is killed on launch
with no output at all — an exit 137 with an empty log is this, every time.

## Layout

```
cmd/ptv-mcp      flags, key handling, database lifecycle, server startup
internal/tools   the eight tools; one register* function each
internal/render  library results -> the shapes tools return
```

## Things that will bite

**Tool descriptions are load-bearing.** The `Description` field and the
`jsonschema` struct tags are the only thing the model reads when deciding which
tool to call and what to pass. Editing that prose changes behaviour as surely as
editing the code, and there is no test that will catch a regression. Treat them
as interface, not documentation: say when to reach for a tool, not only what it
does, and name the argument that is easy to omit.

**`internal/render` is deliberately duplicated.** A near-copy backs the web front
end. Do not extract it into a shared module: presentation differs per client, and
coupling two separately deployed apps through one wire format costs more than it
buys. If the library grows a field that matters, add it in both places.

**Times are absolute, always.** Tools return `08:16`, never "in 3 minutes". A
reply is read some time after it is generated and a countdown is wrong by then.

**Status values are machine names.** `on_time`, `late`, `cancelled`, `unknown` —
the client phrases them. Do not return prose the model has to parse back.

**"No live data" is not "on time".** Realtime reaches about an hour ahead, so
most of the schedule carries no prediction. Anything that cannot determine a
status must say so rather than defaulting to reassurance.

**Never commit the database or a key.** `*.db`, `*.key` and `*.mcpb` are ignored;
keep it that way. The realtime key belongs in the environment, the client config,
or `realtime.key` beside the database.

## Changing the library alongside this

`ptv-gtfs-go` is consumed as a tagged module, not a `replace`. To use unreleased
library changes, tag a new version there first and bump `go.mod` here — a
`replace` pointing at a local checkout breaks `go install` for everyone and must
not be committed.

## Commits

One commit on `main`, amended rather than added to. Author and committer are the
repo owner's GitHub noreply address. No `Co-Authored-By` lines.
