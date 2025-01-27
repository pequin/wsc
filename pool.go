package wsc

import (
	"sync"
	"time"
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

type Pool endpoints
type endpoints struct {
	mtx sync.Mutex
	cnd *sync.Cond
	wgp sync.WaitGroup // Ticker wait group.
	srt chan struct{}  // Incoming message channel for stop reconnect ticker.
	trc *time.Ticker   // The ticker for reconnect.
	prs bool           // The pool is currently in a reconnecting state.
	eds []*endpoint
}

func (p *Pool) Bind(new Endpoint) {

	if p.cnd == nil {
		p.cnd = sync.NewCond(&p.mtx)
	}

	p.mtx.Lock()
	if p.prs {
		p.cnd.Wait()
	}
	p.mtx.Unlock()

	e := new.(*endpoint)

	// Stopping reconnect goroutine.
	if e.trc != nil {
		p.mtx.Lock()
		e.isp = true
		p.mtx.Unlock()
		e.srt <- struct{}{}
		e.tgp.Wait()

		p.mtx.Lock()
		close(e.srt)
		e.trc = nil
		p.mtx.Unlock()
	}

	p.eds = append(p.eds, e)
}

// Regular reconnection.
func (p *Pool) Reconnect(evry time.Duration) {

	p.srt = make(chan struct{})

	if p.trc == nil {
		p.trc = time.NewTicker(evry)
	} else {
		p.trc.Reset(evry)
	}

	p.wgp.Add(1)

	go func() {

	done:
		for {
			select {
			case <-p.trc.C:

				p.mtx.Lock()
				p.prs = true
				p.mtx.Unlock()

				for i := 0; i < len(p.eds); i++ {
					p.eds[i].reconnect()
				}

				p.mtx.Lock()
				p.prs = false
				p.cnd.Broadcast()
				p.mtx.Unlock()

			case <-p.srt:
				p.trc.Stop()
				break done
			}
		}

		p.wgp.Done()

	}()
}

// Unbind all endpoints.
func (p *Pool) Unbind() {

	// Stopping reconnect goroutine.
	if p.trc != nil {
		p.srt <- struct{}{}
		p.wgp.Wait()
		close(p.srt)
		p.trc = nil
	}

	for i := 0; i < len(p.eds); i++ {
		p.mtx.Lock()
		p.eds[i].isp = false
		p.mtx.Unlock()
	}
}
