package wsc

import (
	"context"
	"log"
	"sync"
)

// Copyright 2025 Vasiliy Vdovin

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

// http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

type WebSocket interface {
	Request(payload []byte) []byte
	Reconnect()
	Close()
}
type websocket struct {
	url        string
	wait       chan struct{}
	cancel     context.CancelFunc
	connection struct {
		request  chan []byte
		response chan []byte
		stream   chan []byte
	}

	flow struct {
		mutex        sync.Mutex
		willBeClosed bool
	}
}

func NewWebSocket(url string, stream func(payload []byte)) WebSocket {

	ws := &websocket{
		url:  url,
		wait: make(chan struct{}),
		connection: struct {
			request  chan []byte
			response chan []byte
			stream   chan []byte
		}{
			request:  make(chan []byte),
			response: make(chan []byte),
			stream:   make(chan []byte),
		},
		flow: struct {
			mutex        sync.Mutex
			willBeClosed bool
		}{
			willBeClosed: false,
		},
	}

	ws.dial()

	go func() {
		for s := range ws.connection.stream {

			stream(s)
		}
	}()

	return ws
}

func (ws *websocket) Request(payload []byte) []byte {

	ws.flow.mutex.Lock()
	defer ws.flow.mutex.Unlock()
	if ws.flow.willBeClosed {
		return nil
	}

	ws.connection.request <- payload

	return <-ws.connection.response
}

func (ws *websocket) Reconnect() {

	ws.cancel()
	<-ws.wait
	ws.dial()
}
func (ws *websocket) Close() {

	ws.flow.mutex.Lock()
	defer ws.flow.mutex.Unlock()
	ws.flow.willBeClosed = true

	ws.cancel()
	<-ws.wait
	close(ws.connection.request)
	close(ws.connection.response)
	close(ws.connection.stream)
	close(ws.wait)

}

func (ws *websocket) dial() {

	go func() {
		var ctx context.Context
		ctx, ws.cancel = context.WithCancel(context.Background())

		if err := dial(ctx, ws.url, ws.connection.request, ws.connection.response, ws.connection.stream); err != nil {
			log.Fatalln(err)
		}

		defer func() {
			ws.wait <- struct{}{}
		}()
	}()
}
