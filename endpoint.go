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

type Config struct {
	// Set delay between sending messages.
	SendLimit time.Duration
}

type Endpoint interface {
	// Connect to the endpoint and start listening incoming messages.
	Listen(incoming func(message []byte))

	// Send text message to server.
	Send(message []byte)

	// Disconnect from server.
	Disconnect()

	// Regular reconnection.
	Reconnect(evry time.Duration)

	// Subscribe to error handling.
	Error(handler func(err error))
}

type endpoint struct {
	url string
	ctx context.Context
	cfc context.CancelFunc
	con *tls.Conn
	wfm []byte           // Write frame.
	wmk []byte           // Write msk.
	wpl int              // Write payload length.
	wer error            // Write error.
	trc *time.Ticker     // The ticker for reconnect.
	lmc []byte           // last message from client.
	omb [][]byte         // Outgoing message buffer.
	srt chan struct{}    // Incoming message channel for stop reconnect ticker.
	imc chan []byte      // Incoming message channel.
	imh func(msg []byte) // Incoming message handler.
	ehl func(err error)  // Error handler handler.
	tgp sync.WaitGroup   // Ticker wait group.
	wgp sync.WaitGroup   // Main wait group.
	cwe time.Time        // The time the connection was established.
	lsa time.Time        // Time of last successful activity.
	lsd time.Time        // Time of last send.
	isp bool             // This connection is in the pool.
	cia bool             // The connection is active.
	rcn bool             // The connection is currently in a reconnecting state.
	ige bool             // Ignore errors.
	cnf *Config
}

func New(url string, config *Config) Endpoint {

	end := &endpoint{
		url: url,
		wfm: []byte{0, 0},
		wmk: []byte{0, 0, 0, 0},
		omb: make([][]byte, 0),
		isp: false,
		cia: false,
		rcn: false,
		ige: false,
		cnf: config,
	}

	return end
}

// Connect to the endpoint and start listening incoming messages.
func (e *endpoint) Listen(incoming func(message []byte)) {

	if e.con != nil {
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
		e.omb = append(e.omb, message)
	} else {
		e.send(0x1, message)
	}
	e.lmc = message
}

// Regular reconnection.
func (e *endpoint) Reconnect(evry time.Duration) {

	if !e.cia || e.trc != nil || e.isp {
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
				e.reconnect()
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

	e.cfc()
	e.cia = false

	// Send message about closing connection.
	e.send(0x8, nil)

	e.ige = true
	e.con.Close()

	e.wgp.Wait()

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

}

// Subscribe to error handling.
func (e *endpoint) Error(handler func(err error)) {
	e.ehl = handler
}

func (e *endpoint) reconnect() {

	if e.rcn || !e.cia || e.con == nil {
		return
	}

	e.rcn = true

	// Ping message.
	png := []byte("reconnect" + time.Now().UTC().String())

	// Incoming messages of the old connection and the new one.
	imc := make(chan []byte)

	c, err := dial(e.url)
	if err != nil {
		e.error(err)
	}

	e.ige = true
	e.con.SetDeadline(time.Now().UTC())
	e.cfc()

	e.wgp.Wait()
	e.wgp.Add(3)
	e.ctx, e.cfc = context.WithCancel(context.Background())

	con := e.con
	e.con = c
	e.ige = false

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
				e.con.SetDeadline(time.Now().UTC())
				con.Close()
			}
		}

		buf = nil
		swg.Done()
	}()

	if len(e.lmc) > 0 {
		e.send(0x1, e.lmc)
	}

	e.send(0x9, png)

	err = write(con, 0x9, png)
	if err != nil {
		e.error(err)
	}

	wgp.Wait()
	close(imc)

	swg.Wait()

	e.con.SetDeadline(time.Time{})
	e.rcn = false
	e.cwe = time.Now().UTC()
	e.lsa = time.Now().UTC()

	omb := e.omb
	e.omb = make([][]byte, 0)

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

	if e.cnf != nil {
		if e.cnf.SendLimit > 0 {
			time.Sleep(e.cnf.SendLimit - time.Now().UTC().Sub(e.lsd))
		}
	}

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
	e.lsd = time.Now().UTC()

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

				ErrClosedConn = fmt.Errorf("disconnected from: %s %s", e.url, string(msg))
				e.error(ErrClosedConn)
				// e.Disconnect()
			}

			e.lsa = time.Now().UTC()

		})

		if err != nil {
			e.error(err)
		}
		e.wgp.Done()

	}()
}
