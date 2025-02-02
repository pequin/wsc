# WebSocket Client

### Implementation of [WebSocket](http://www.rfc-editor.org/rfc/rfc6455.txt) client in [Go](http://golang.org/)


### Installation
```shell
go get github.com/pequin/wsc
```
### Creating a basic connection:
```go
import (
    "github.com/pequin/wsc"
)

func main() {
    endpoint := wsc.New("fstream.binance.com/stream/")
        
    // Connect to the endpoint and start listening incoming messages.
    endpoint.Listen(func(message []byte) {
		fmt.Println(string(message))
	})

    // Send text message to server.
    endpoint.Send([]byte(`{"method": "SUBSCRIBE","params": ["btcusdt@aggTrade"],"id": 1}`))
}
```

### Error handling
In order to be able to handle errors during work, you need to subscribe to the error handler.
If this is not done, any error that occurs during execution will lead to: os.Exit(1).
```go
// Custom error handler.
endpoint.Error(func(err error) {
	log.Println("err", err)
})
```
### Schedule a seamless reconnection.

#### When reconnect, data is not lost!
in this example, a reconnect will be performed every 24 hours.
```go
// Schedule a seamless regular reconnection.
endpoint.Reconnect(time.Hour*24)
```


Example of a real reconnection to the binance server.

> **_NOTE:_** \
f: is first ID\
l: is last ID

```shell
{"stream":"btcusdt@aggTrade","data":{"e":"aggTrade","E":1737917715466,"a":2541859789,"s":"BTCUSDT","p":"105092.10","q":"0.054","f":5897988206,"l":5897988212,"T":1737917715312,"m":false}}
reconnect will be start
{"stream":"btcusdt@aggTrade","data":{"e":"aggTrade","E":1737917715759,"a":2541859790,"s":"BTCUSDT","p":"105092.10","q":"0.021","f":5897988213,"l":5897988214,"T":1737917715640,"m":false}}
{"stream":"btcusdt@aggTrade","data":{"e":"aggTrade","E":1737917715762,"a":2541859791,"s":"BTCUSDT","p":"105092.00","q":"0.001","f":5897988215,"l":5897988215,"T":1737917715755,"m":true}}
{"stream":"btcusdt@aggTrade","data":{"e":"aggTrade","E":1737917715913,"a":2541859792,"s":"BTCUSDT","p":"105092.10","q":"0.050","f":5897988216,"l":5897988217,"T":1737917715758,"m":false}}
{"stream":"btcusdt@aggTrade","data":{"e":"aggTrade","E":1737917716102,"a":2541859793,"s":"BTCUSDT","p":"105092.10","q":"0.001","f":5897988218,"l":5897988218,"T":1737917715946,"m":false}}
{"stream":"btcusdt@aggTrade","data":{"e":"aggTrade","E":1737917716276,"a":2541859794,"s":"BTCUSDT","p":"105092.00","q":"0.007","f":5897988219,"l":5897988219,"T":1737917716121,"m":true}}
reconnection was completed
{"result":null,"id":1}
{"stream":"btcusdt@aggTrade","data":{"e":"aggTrade","E":1737917717026,"a":2541859795,"s":"BTCUSDT","p":"105092.00","q":"0.060","f":5897988220,"l":5897988224,"T":1737917716871,"m":true}}
```





### Disconnect
Sends a message to the server to close the connection and stops all active processes associated with this connection.
```go
// Disconnect from server.
endpoint.Disconnect()
```
> **_NOTE:_** 
After disconnecting, the connection can be reused.
```go
// Disconnect from server.
endpoint.Disconnect()

// Connect to the endpoint and start listening incoming messages.
endpoint.Listen(func(message []byte) {
	fmt.Println(string(message))
})

// Send text message to server.
endpoint.Send([]byte(`{"method": "SUBSCRIBE","params": ["btcusdt@aggTrade"],"id": 1}`))
```

### Full example of usage

```go
import (
    "github.com/pequin/wsc"
)

func main() {
    endpoint := wsc.New("fstream.binance.com/stream/")
        
    // Connect to the endpoint and start listening incoming messages.
    endpoint.Listen(func(message []byte) {
		fmt.Println(string(message))
	})

    // Send text message to server.
    endpoint.Send([]byte(`{"method": "SUBSCRIBE","params": ["btcusdt@aggTrade"],"id": 1}`))

    // Schedule a seamless regular reconnection.
    endpoint.Reconnect(time.Hour*24)

    time.Sleep(time.Minute)

    // Disconnect from server.
    endpoint.Disconnect()
}
```

## Pool - combines endpoints

In this example, the Reconnect control is taken over by Poll.
If multiple connections are created and they require regular reconnection, to avoid creating one-dimensional reconnections, Pool will be executed reconnections one after another.
```go
var pool wsc.Pool

endpoint1 := wsc.New("fstream.binance.com/stream/")
endpoint2 := wsc.New("stream.binance.com/stream")

pool.Bind(endpoint1)
pool.Bind(endpoint2)
pool.Reconnect(time.Second)

```

Listening of all connections.

```go
var pool wsc.Pool

endpoint1 := wsc.New("fstream.binance.com/stream/")
endpoint2 := wsc.New("stream.binance.com/stream")

pool.Bind(endpoint1)
pool.Bind(endpoint2)
pool.Listen(func(message []byte) {
	fmt.Println(string(message))
})

```

Unbinds all endpoints.
```go
pool.Unbind()
```
