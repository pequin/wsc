# wsc

A small WebSocket client for Go. It speaks RFC 6455 over TLS, uses only the standard library,
and lets you call a WebSocket API like a function: send a request, get the reply, and get
everything else the server pushes in a separate callback.

```sh
go get github.com/pequin/wsc
```

Requires Go 1.26 or newer.

## Usage

```go
package main

import (
	"fmt"

	"github.com/pequin/wsc"
)

type pingRequest struct {
	Method string `json:"method"`
}

type pingResponse struct {
	Result string `json:"result"`
}

func main() {
	ws := wsc.NewWebSocket("wss://stream.example.com/ws")

	// Messages the server pushes on its own.
	ws.Listen(func(msg []byte) {
		fmt.Println("pushed:", string(msg))
	})

	ws.Connect()
	defer ws.Disconnect()

	// Raw request and reply.
	reply := ws.Request([]byte(`{"method":"ping"}`))
	fmt.Println("reply:", string(reply))

	// Typed endpoint: JSON in, JSON out.
	ping := wsc.NewEndpoint[pingRequest, pingResponse](ws)
	fmt.Println(ping.Send(pingRequest{Method: "ping"}).Result)
}
```

## How it works

- `Connect` starts the connection in the background: a TLS connection to port 443, the opening
  handshake, and a check that a `Sec-WebSocket-Accept` sent by the server matches the key.
- `Request` sends a text frame and treats the next text message from the server as its reply.
  Requests are serialized, one at a time. Every other message goes to the `Listen` callback.
- `NewEndpoint` wraps `Request` with JSON: `Send` marshals the request value and unmarshals the
  reply into the response type.
- Outgoing frames are masked as the RFC requires, and pings are answered with pongs.
- `Reconnect` closes the connection and opens a new one. `Disconnect` sends a Close frame
  and closes the connection.

## Limits

This is a v0 library and the API will change. Known limits of the current code:

- Every URL is dialed over TLS on port 443: the scheme is not checked, an explicit port in the
  URL is not supported yet, and a leading `www.` is removed from the host name.
- Text frames only. Binary and fragmented messages are not supported.
- A reply is matched by order, not by an ID, so it fits APIs that answer each request before
  pushing anything else.
- Errors, including a normal close from the server, stop the program with `log.Fatal`.
- The slice passed to `Listen` and returned by `Request` shares the reader's buffer and can be
  overwritten by the next message.
- A push that arrives while a `Request` is waiting can block both of them.

Fixes for these are planned on the `dev` branch.

## Development

```sh
gofmt -l . && go vet ./...
go test -race ./...
golangci-lint run ./...
govulncheck ./...
```

The tests run against an in-process RFC 6455 test server, so they need no network. CI runs
vet and the race tests on Go 1.26 and 1.27, plus gofmt, golangci-lint and govulncheck. Until
the existing lint findings are fixed, golangci-lint in CI fails only on new issues.

## License

Apache 2.0, see [LICENSE](LICENSE).
