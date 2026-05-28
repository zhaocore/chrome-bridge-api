# agent-api

Go implementation of the Agent WebBridge local daemon. It exposes an HTTP API for agents and a WebSocket bridge for the Chrome extension under `agent-chrome-plugin`.

## Requirements

- Go 1.21+
- Chrome extension loaded from `../agent-chrome-plugin/dist`

## Build

Default port is compiled into the binary. If not overridden, it is `10086`.

```bash
go build -o agent-webbridge ./cmd/agent-webbridge
```

Override the port at build time:

```bash
go build -ldflags "-X main.defaultPort=10087" -o agent-webbridge ./cmd/agent-webbridge
```

## Run

Start the daemon in the background:

```bash
./agent-webbridge start
```

Check status:

```bash
./agent-webbridge status
```

Stop or restart:

```bash
./agent-webbridge stop
./agent-webbridge restart
```

Read logs:

```bash
./agent-webbridge logs -n 100
./agent-webbridge logs -f
./agent-webbridge logs --prev
```

## API

For the default port:

- `GET http://127.0.0.1:10086/status`
- `POST http://127.0.0.1:10086/command`
- `GET http://127.0.0.1:10086/tools`
- `POST http://127.0.0.1:10086/api/connections`
- `ws://127.0.0.1:10086/ws`

Example command:

```bash
curl -s -X POST http://127.0.0.1:10086/command \
  -H 'Content-Type: application/json' \
  -d '{"action":"navigate","args":{"url":"https://example.com","newTab":true},"session":"demo"}'
```

## Test

```bash
GOTOOLCHAIN=local go test ./...
```

## More Detail

See [docs/technical-plan.md](docs/technical-plan.md) for protocol details, tool arguments, and implementation notes.
