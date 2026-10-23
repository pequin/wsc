// Copyright 2025 Vasiliy Vdovin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package wsc

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Frame opcodes from RFC 6455 §5.2.
const (
	opText  byte = 0x1
	opClose byte = 0x8
	opPing  byte = 0x9
	opPong  byte = 0xA
)

// acceptGUID is appended to Sec-WebSocket-Key before hashing it into
// Sec-WebSocket-Accept (RFC 6455 §1.3).
const acceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type frame struct {
	fin     bool
	opcode  byte
	payload []byte
}

func dial(ctx context.Context, rawURL string, request <-chan []byte, response chan<- []byte, stream chan<- []byte) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}

	host, _ := strings.CutPrefix(u.Host, "www.")
	path := u.RequestURI()
	port := u.Port()
	if port == "" {
		port = "443"
	}

	key, err := newKey(rand.Reader)
	if err != nil {
		return err
	}

	conn, err := tls.Dial("tcp", fmt.Sprintf("%s:%s", host, port), nil)
	if err != nil {
		return err
	}
	if err = conn.Handshake(); err != nil {
		return err
	}
	if err = conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	if err = handshake(conn, host, path, key); err != nil {
		return err
	}
	if err = conn.SetDeadline(time.Time{}); err != nil {
		return err
	}

	return serve(ctx, conn, request, response, stream)
}

// acceptKey returns the Sec-WebSocket-Accept value a server must send back
// for the given Sec-WebSocket-Key.
func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + acceptGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// newKey returns a base64-encoded Sec-WebSocket-Key built from 16 bytes read
// from random (RFC 6455 §4.1).
func newKey(random io.Reader) (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(random, b[:]); err != nil {
		return "", fmt.Errorf("generate Sec-WebSocket-Key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b[:]), nil
}

// handshake sends the opening handshake for path on host over rw and checks
// the server's reply. key is the base64-encoded Sec-WebSocket-Key.
func handshake(rw io.ReadWriter, host, path, key string) error {
	if _, err := fmt.Fprintf(rw, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n", path, host, key); err != nil {
		return err
	}

	br := bufio.NewReader(rw)
	status, err := br.ReadString('\n')
	switch {
	case err != nil:
		return err
	case strings.Contains(status, "404 Not Found"):
		return fmt.Errorf("%s: the path %s to is wrong", host, path)
	case !strings.Contains(status, "101 Switching"):
		return fmt.Errorf("%s: the server did not switch to the websocket protocol", host)
	}

	// Header lines are read up to '\r', so every line after the status line
	// starts with the '\n' left over from the previous one.
	for {
		line, err := br.ReadString('\r')
		if err != nil {
			return err
		}

		line = strings.ReplaceAll(strings.ReplaceAll(line, "\n", ""), "\r", "")
		if line == "" {
			return nil
		}

		name, value, ok := strings.Cut(line, ": ")
		if !ok {
			return fmt.Errorf("%s: the server did not switch to the websocket protocol", host)
		}
		if strings.ToLower(name) == "sec-websocket-accept" && value != acceptKey(key) {
			return fmt.Errorf("%s: invalid Sec-WebSocket-Accept", host)
		}
	}
}

// readFrame reads one frame from r. The payload is read into buf when buf has
// enough capacity, so it stays valid only until buf is reused.
func readFrame(r io.Reader, buf []byte) (frame, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:2]); err != nil {
		return frame{}, err
	}

	f := frame{
		fin:    hdr[0]&0x80 != 0,
		opcode: hdr[0] & 0x0F,
	}

	n := int(hdr[1] & 0x7F)
	switch n {
	case 126:
		if _, err := io.ReadFull(r, hdr[:2]); err != nil {
			return frame{}, err
		}
		n = int(binary.BigEndian.Uint16(hdr[:2]))
	case 127:
		if _, err := io.ReadFull(r, hdr[:8]); err != nil {
			return frame{}, err
		}
		n = int(binary.BigEndian.Uint64(hdr[:8]))
	}

	if cap(buf) < n {
		buf = make([]byte, 0, n)
	}
	f.payload = buf[:n]
	if _, err := io.ReadFull(r, f.payload); err != nil {
		return frame{}, err
	}
	return f, nil
}

// writeFrame writes payload to w as a single final frame. Only text, close
// and pong frames are supported.
func writeFrame(w io.Writer, opcode byte, payload []byte) error {
	if opcode != opText && opcode != opClose && opcode != opPong {
		return errors.New("frame type not supported by client")
	}

	// A client must mask every frame it sends (RFC 6455 §5.3), hence the
	// 0x80 mask bit in the length byte.
	n := len(payload)
	b := make([]byte, 0, 14+n)
	b = append(b, 0x80|opcode)
	switch {
	case n <= 125:
		b = append(b, 0x80|byte(n))
	case n <= 65535:
		b = append(b, 0x80|126)
		b = binary.BigEndian.AppendUint16(b, uint16(n))
	default:
		b = append(b, 0x80|127)
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}

	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	b = append(b, mask[:]...)
	for i, c := range payload {
		b = append(b, c^mask[i%4])
	}

	_, err := w.Write(b)
	return err
}

// serve runs an established connection until ctx is canceled. Text messages
// from the server go to stream, except the first one after each request,
// which goes to response.
func serve(ctx context.Context, conn net.Conn, request <-chan []byte, response chan<- []byte, stream chan<- []byte) error {
	messages := make(chan []byte)
	readerDone := make(chan error)
	frames := make(chan frame)
	written := make(chan struct{})
	writerDone := make(chan error)

	go func() {
		var err error
		var f frame
		buf := make([]byte, 0, 256)
	loop:
		for {
			if f, err = readFrame(conn, buf); err != nil {
				break
			}
			buf = f.payload

			switch f.opcode {
			case opText:
				messages <- f.payload
			case opClose:
				err = errors.New("the server closed the connection")
				break loop
			case opPing:
				frames <- frame{opcode: opPong, payload: f.payload}
				<-written
			default:
				err = errors.New("frame type not supported by client")
				break loop
			}

			if !f.fin {
				err = errors.New("the final frame was not received")
				break
			}
		}
		readerDone <- err
	}()

	go func() {
		var err error
		for f := range frames {
			if err = writeFrame(conn, f.opcode, f.payload); err != nil {
				break
			}
			if f.opcode == opClose {
				break
			}
			written <- struct{}{}
		}
		writerDone <- err
	}()

	var exchange sync.WaitGroup
	exchange.Go(func() {
		for {
			select {
			case msg := <-messages:
				stream <- msg
			case msg := <-request:
				frames <- frame{opcode: opText, payload: msg}
				<-written
				response <- <-messages
			case <-ctx.Done():
				return
			}
		}
	})

	select {
	case <-ctx.Done():
	case err := <-writerDone:
		log.Fatalln(err)
	case err := <-readerDone:
		log.Fatalln(err)
	}

	// Let an in-flight request finish before sending Close.
	exchange.Wait()
	frames <- frame{opcode: opClose}
	if err := <-writerDone; err != nil {
		log.Fatalln(err)
	}

	// The expired deadline unblocks the reader, so it is expected to stop
	// with a timeout.
	if err := conn.SetDeadline(time.Now()); err != nil {
		log.Fatalln(err)
	}
	err := <-readerDone
	if err, ok := err.(net.Error); !ok || !err.Timeout() {
		log.Fatalln(err)
	}

	return conn.Close()
}
