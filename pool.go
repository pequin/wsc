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
	lwg sync.WaitGroup // Listen wait group.
	twg sync.WaitGroup // Ticker wait group.
	stp chan struct{}  // Stop.
	trc *time.Ticker   // The ticker for reconnect.
	prs bool           // The pool is currently in a reconnecting state.
	eds []*endpoint
}

func (p *Pool) Bind(new Endpoint) {

	if p.cnd == nil {
		p.cnd = sync.NewCond(&p.mtx)
		p.stp = make(chan struct{})
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

func (p *Pool) Listen(incoming func(message []byte)) {

	msg := make(chan []byte) // Messages channel.

	p.lwg.Add(1)

	go func() {

	done:
		for {

			select {
			default:
				incoming(<-msg)
			case <-p.stp:
				break done
			}

		}

		p.lwg.Done()
	}()

	for i := 0; i < len(p.eds); i++ {

		p.mtx.Lock()

		if i < len(p.eds)-1 {
			time.Sleep(time.Minute)
		}

		if p.eds[i].imh == nil {
			p.eds[i].Listen(func(message []byte) {
				msg <- message
			})
		} else {
			p.eds[i].imh = func(message []byte) {
				msg <- message
			}
		}

		p.mtx.Unlock()
	}
}

// Regular reconnection.
func (p *Pool) Reconnect(evry time.Duration) {

	if p.trc == nil {
		p.trc = time.NewTicker(evry)
	} else {
		p.trc.Reset(evry)
	}

	p.twg.Add(1)

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

			case <-p.stp:
				p.trc.Stop()
				break done
			}
		}

		p.twg.Done()

	}()
}

// Unbind all endpoints.
func (p *Pool) Unbind() {

	if p.cnd == nil {
		p.stp <- struct{}{}
	}

	// Stopping reconnect goroutine.
	if p.trc != nil {
		p.twg.Wait()
		p.trc = nil
	}

	p.lwg.Wait()

	for i := 0; i < len(p.eds); i++ {
		p.mtx.Lock()
		p.eds[i].isp = false
		p.mtx.Unlock()
	}

}
