package wsc

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

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"log"
	"sync"
	"time"
)

type Endpoint interface {
	// Connect to the endpoint and start listening incoming messages.
	Listen(incoming func(message []byte))

	// Send text message to server.
	Send(message []byte)

	// Disconnect from server..
	Disconnect()

	// Regular reconnection..
	Reconnect(evry time.Duration, message []byte)

	// Subscribe to error handling.
	Error(handler func(err error))
}

type endpoint struct {
	url string
	ctx context.Context
	cfc context.CancelFunc
	con *tls.Conn
	wfm []byte // Write frame.
	wmk []byte // Write msk.
	wpl int    // Write payload length.
	wer error  // Write error.
	mtx sync.Mutex
	trc *time.Ticker     // The ticker for reconnect.
	omb [][]byte         // Outgoing message buffer.
	srt chan struct{}    // Incoming message channel for stop reconnect ticker .
	imc chan []byte      // Incoming message channel.
	imh func(msg []byte) // Incoming message handler.
	ehl func(err error)  // Error handler handler.
	tgp sync.WaitGroup   // Ticker wait group.
	wgp sync.WaitGroup   // Main wait group.
	cwe time.Time        // The time the connection was established.
	lsa time.Time        // Time of last successful activity.
	cia bool             // The connection is active.
	rcn bool             // The connection is currently in a reconnecting state.
	ige bool             // Ignore errors.

}

func New(url string) Endpoint {

	end := &endpoint{
		url: url,
		wfm: []byte{0, 0},
		wmk: []byte{0, 0, 0, 0},
		omb: make([][]byte, 0),
		cia: false,
		rcn: false,
		ige: false,
	}

	return end
}

// Connect to the endpoint and start listening incoming messages.
func (e *endpoint) Listen(incoming func(message []byte)) {
	e.mtx.Lock()
	if e.con != nil {
		e.mtx.Unlock()
		return
	}

	con, err := dial(e.url)
	if err != nil {
		e.error(err)
	}

	e.con = con
	e.imh = incoming
	e.ctx, e.cfc = context.WithCancel(context.Background())
	e.imc = make(chan []byte, 1024)
	e.cwe = time.Now().UTC()
	e.lsa = time.Now().UTC()
	e.cia = true
	e.rcn = false
	e.ige = false
	e.mtx.Unlock()

	e.wgp.Add(3)
	e.listener()
	e.pulse()

	// If messages were sent after disconnection.
	if len(e.omb) > 0 {
		if len(e.omb) > 0 {
			for i := 0; i < len(e.omb); i++ {
				e.send(0x1, e.omb[i])
			}
			e.omb = make([][]byte, 0)
		}
	}
}

// Send text message to server.
func (e *endpoint) Send(message []byte) {
	if !e.cia || e.rcn || e.con == nil {
		e.mtx.Lock()
		e.omb = append(e.omb, message)
		e.mtx.Unlock()
	} else {
		e.send(0x1, message)
	}
}

// Regular reconnection.
func (e *endpoint) Reconnect(evry time.Duration, message []byte) {

	if !e.cia && e.trc != nil {
		return
	}

	e.srt = make(chan struct{})

	e.trc = time.NewTicker(evry)

	e.tgp.Add(1)

	go func() {

	done:
		for {

			select {
			case <-e.trc.C:
				e.reconnect(message)
			case <-e.srt:
				e.trc.Stop()
				break done
			}

		}

		e.tgp.Done()

	}()
}

// Disconnect from server.
func (e *endpoint) Disconnect() {

	if e.con == nil {
		return
	}

	// Stopping reconnect goroutine.
	if e.trc != nil {
		e.srt <- struct{}{}
		e.tgp.Wait()
		close(e.srt)
		e.trc = nil
	}

	e.mtx.Lock()
	e.cfc()
	e.cia = false
	e.mtx.Unlock()

	// Send message about closing connection.
	e.send(0x8, nil)

	e.mtx.Lock()
	e.ige = true
	e.con.Close()
	e.mtx.Unlock()

	e.wgp.Wait()

	e.mtx.Lock()
	e.ctx = nil
	e.cfc = nil
	e.con = nil
	e.imh = nil
	e.cwe = time.Time{}
	e.lsa = time.Time{}
	e.cia = false
	e.rcn = false
	e.ige = false

	close(e.imc)

	e.mtx.Unlock()
}

// Subscribe to error handling.
func (e *endpoint) Error(handler func(err error)) {
	e.mtx.Lock()
	e.ehl = handler
	e.mtx.Unlock()
}

func (e *endpoint) reconnect(message []byte) {

	if e.rcn || !e.cia || e.con == nil {
		return
	}

	e.mtx.Lock()
	e.rcn = true
	e.mtx.Unlock()

	// Ping message.
	png := []byte("reconnect" + time.Now().UTC().String())

	// Incoming messages of the old connection and the new one.
	imc := make(chan []byte)

	c, err := dial(e.url)
	if err != nil {
		e.error(err)
	}

	e.mtx.Lock()
	e.ige = true
	e.con.SetDeadline(time.Now().UTC())
	e.mtx.Unlock()
	e.cfc()

	e.wgp.Wait()
	e.wgp.Add(3)
	e.ctx, e.cfc = context.WithCancel(context.Background())

	e.mtx.Lock()
	con := e.con
	e.con = c
	e.ige = false
	e.mtx.Unlock()

	// Old connection wait group
	var wgp sync.WaitGroup
	wgp.Add(2)

	// Old connection listener.
	con.SetDeadline(time.Time{})
	go func() {
		read(con, func(fin bool, opc byte, msg []byte) {
			if fin && (opc == 0x1 || opc == 0x2 || opc == 0xA) {
				imc <- msg
			}
		})

		wgp.Done()
	}()

	// New connection listener.
	go func() {
		read(e.con, func(fin bool, opc byte, msg []byte) {
			if fin && (opc == 0x1 || opc == 0x2 || opc == 0xA) {
				imc <- msg
			} else if fin && opc == 0x9 {
				e.send(0xA, msg)
			}

		})

		wgp.Done()
	}()

	// Lisen messages from old and new connections.
	var swg sync.WaitGroup
	swg.Add(1)
	go func() {

		var unq bool
		buf := make([][]byte, 0)

		for imc := range imc {

			unq = true

			for i := 0; i < len(buf); i++ {

				if bytes.Equal(buf[i], imc) {
					unq = false
					break
				}
			}

			if unq && !bytes.Equal(png, imc) {
				e.imc <- imc
			}

			if unq {
				buf = append(buf, imc)
			} else {
				e.mtx.Lock()
				e.con.SetDeadline(time.Now().UTC())
				con.Close()
				e.mtx.Unlock()
			}
		}

		buf = nil
		swg.Done()
	}()

	if len(message) > 0 {
		e.send(0x1, message)
	}

	e.send(0x9, png)

	e.mtx.Lock()
	err = write(con, 0x9, png)
	e.mtx.Unlock()
	if err != nil {
		e.error(err)
	}

	wgp.Wait()
	close(imc)

	swg.Wait()

	e.mtx.Lock()
	e.con.SetDeadline(time.Time{})
	e.rcn = false
	e.cwe = time.Now().UTC()
	e.lsa = time.Now().UTC()

	omb := e.omb
	e.omb = make([][]byte, 0)
	e.mtx.Unlock()

	e.listener()
	e.pulse()

	if len(omb) > 0 {
		for i := 0; i < len(omb); i++ {
			e.send(0x1, omb[i])
		}
	}

	omb = nil
}

func (e *endpoint) error(message error) {
	if e.ehl == nil && !e.ige {
		log.Fatalln(message)
	} else if !e.ige {
		e.ehl(message)
	}

}

func (e *endpoint) pulse() {

	// Keep alive.
	go func() {
		plt := time.NewTicker(time.Second)

	done:
		for {

			select {
			case tcr := <-plt.C:

				if e.cia && !e.rcn && tcr.Sub(e.lsa).Round(time.Second) > time.Second {
					e.error(ErrLostConn)

				} else if e.cia && !e.rcn && tcr.Sub(e.lsa).Round(time.Second) == time.Second {
					e.send(0x9, []byte(e.lsa.String()))
				}

			case <-e.ctx.Done():
				break done
			}

		}

		plt.Stop()
		plt = nil
		e.wgp.Done()

	}()

	// Incoming messages.
	go func() {
	done:
		for {
			select {
			case imc := <-e.imc:
				if e.imh != nil {
					e.imh(imc)
				}
			case <-e.ctx.Done():
				break done
			}
		}
		e.wgp.Done()
	}()
}

func (e *endpoint) send(opc byte, pld []byte) {

	e.mtx.Lock()
	e.wpl = len(pld)

	e.wfm[0] = (1 << 7) | byte(opc)
	if e.wpl <= 125 {
		e.wfm[1] = byte(0x80) | byte(e.wpl)
	} else if e.wpl <= 65535 {
		e.wfm[1] = byte(0x80) | byte(126)
		e.wfm = append(e.wfm, byte(e.wpl>>8), byte(e.wpl))
	} else {
		e.wfm[1] = byte(0x80) | byte(127) // Mask = 1, len = 127.
		e.wfm = append(e.wfm,
			byte(e.wpl>>56), byte(e.wpl>>48),
			byte(e.wpl>>40), byte(e.wpl>>32),
			byte(e.wpl>>24), byte(e.wpl>>16),
			byte(e.wpl>>8), byte(e.wpl))
	}

	if _, e.wer = rand.Read(e.wmk); e.wer != nil {
		log.Fatalln(e.wer)
	}

	e.wfm = append(e.wfm, e.wmk...)

	for i, b := range pld {
		e.wfm = append(e.wfm, b^e.wmk[i%4])
	}

	_, e.wer = e.con.Write(e.wfm)
	e.wfm = e.wfm[:2]
	e.mtx.Unlock()

	if e.wer != nil {
		e.error(e.wer)
	}
}

func (e *endpoint) listener() {

	go func() {

		err := read(e.con, func(fin bool, opc byte, msg []byte) {
			if fin && (opc == 0x1 || opc == 0x2) {
				e.imc <- msg
			} else if fin && opc == 0x9 {
				e.send(0xA, msg)
			} else if fin && opc == 0x8 {
				e.Disconnect()
				ErrClosedConn = fmt.Errorf("server closed the connection: %s", string(msg))
				e.error(ErrClosedConn)
			}

			e.mtx.Lock()
			e.lsa = time.Now().UTC()
			e.mtx.Unlock()

		})

		if err != nil {
			e.error(err)
		}
		e.wgp.Done()

	}()

}
