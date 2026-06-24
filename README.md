# chrome-bridge-api

Go implementation of the Chrome Bridge local daemon. It exposes an HTTP API for agents and a WebSocket bridge for the Chrome extension under `chrome-bridge-plugin`.

## Requirements

- Go 1.21+
- Chrome extension loaded from `../chrome-bridge-plugin/dist`

## Build

Default port is compiled into the binary. If not overridden, it is `10089`.

```bash
make build
```

Override the port at build time:

```bash
make build PORT=10090
```

## Run

Start the daemon in the background:

```bash
./bin/chrome-bridge start
```

Check status:

```bash
./bin/chrome-bridge status
```

Stop or restart:

```bash
./bin/chrome-bridge stop
./bin/chrome-bridge restart
```

Read logs:

```bash
./bin/chrome-bridge logs -n 100
./bin/chrome-bridge logs -f
./bin/chrome-bridge logs --prev
```

## API

For the default port:

- `GET http://127.0.0.1:10089/status`
- `POST http://127.0.0.1:10089/command`
- `GET http://127.0.0.1:10089/tools`
- `POST http://127.0.0.1:10089/api/connections`
- `ws://127.0.0.1:10089/ws`

Example command:

```bash
curl -s -X POST http://127.0.0.1:10089/command \
  -H 'Content-Type: application/json' \
  -d '{"action":"navigate","args":{"url":"https://example.com","newTab":true},"session":"demo"}'
```

## Test

```bash
make test
```

Equivalent raw Go command:

```bash
GOTOOLCHAIN=local go test ./...
```

## More Detail

See [docs/technical-plan.md](docs/technical-plan.md) for protocol details, tool arguments, and implementation notes.
