package wsc

import (
	"encoding/json"
	"log"
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

type Endpoint[request any, response any] interface {
	Send(request) response
}

type endpoint[request any, response any] struct {
	ws *websocket
}

func NewEndpoint[request any, response any](base WebSocket) Endpoint[request, response] {

	return &endpoint[request, response]{
		ws: base.(*websocket),
	}

}

func (ep *endpoint[request, response]) Send(req request) response {

	var res response

	ep.ws.flow.mutex.Lock()
	defer ep.ws.flow.mutex.Unlock()
	if ep.ws.flow.isClosed {
		log.Fatalf("The Connect() method was not called before the request, to the end point %s", ep.ws.url)
	}
	if ep.ws.flow.isWillBeClosed {
		return res
	}

	if payload, err := json.Marshal(req); err == nil {
		ep.ws.connection.request <- payload
	} else {
		log.Fatalln(err)
	}

	if err := json.Unmarshal(<-ep.ws.connection.response, &res); err != nil {
		log.Fatalln(err)
	}

	return res
}
