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
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestAcceptKey(t *testing.T) {
	// Example from RFC 6455 §1.3.
	const key, want = "dGhlIHNhbXBsZSBub25jZQ==", "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	if got := acceptKey(key); got != want {
		t.Errorf("acceptKey(%q) = %q, want %q", key, got, want)
	}
}

func TestNewKey(t *testing.T) {
	random := bytes.NewReader([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17})
	const want = "AQIDBAUGBwgJCgsMDQ4PEA=="
	got, err := newKey(random)
	if err != nil {
		t.Fatalf("newKey() error = %v", err)
	}
	if got != want {
		t.Errorf("newKey() = %q, want %q", got, want)
	}
}

func TestNewKeyReadError(t *testing.T) {
	errRandom := errors.New("entropy source failed")

	tests := []struct {
		name   string
		random io.Reader
		want   error
	}{
		{name: "read error", random: iotest.ErrReader(errRandom), want: errRandom},
		{name: "short read", random: bytes.NewReader(make([]byte, 15)), want: io.ErrUnexpectedEOF},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := newKey(tt.random)
			if !errors.Is(err, tt.want) {
				t.Errorf("newKey() error = %v, want %v", err, tt.want)
			}
			if key != "" {
				t.Errorf("newKey() = %q, want an empty key on error", key)
			}
		})
	}
}

func TestHandshake(t *testing.T) {
	const host, path, key = "example.com", "/stream?id=7", "dGhlIHNhbXBsZSBub25jZQ=="

	tests := []struct {
		name    string
		reply   string // %s is replaced with Sec-WebSocket-Accept.
		wantErr bool
	}{
		{
			name:  "switching protocols",
			reply: "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
		},
		{
			name:    "wrong accept",
			reply:   "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: x%s\r\n\r\n",
			wantErr: true,
		},
		{
			name:    "not found",
			reply:   "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n",
			wantErr: true,
		},
		{
			name:    "bad request",
			reply:   "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n",
			wantErr: true,
		},
		{
			name:    "malformed header",
			reply:   "HTTP/1.1 101 Switching Protocols\r\nUpgrade websocket\r\n\r\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, server := net.Pipe()
			t.Cleanup(func() {
				_ = client.Close()
				_ = server.Close()
			})
			deadline := time.Now().Add(5 * time.Second)
			if err := errors.Join(client.SetDeadline(deadline), server.SetDeadline(deadline)); err != nil {
				t.Fatal(err)
			}

			done := make(chan struct{})
			go func() {
				defer close(done)

				req, err := http.ReadRequest(bufio.NewReader(server))
				if err != nil {
					t.Errorf("read request: %v", err)
					return
				}
				if req.Host != host || req.URL.RequestURI() != path {
					t.Errorf("request target = %q %q, want %q %q", req.Host, req.URL.RequestURI(), host, path)
				}
				for name, want := range map[string]string{
					"Upgrade":               "websocket",
					"Connection":            "Upgrade",
					"Sec-WebSocket-Version": "13",
					"Sec-WebSocket-Key":     key,
				} {
					if got := req.Header.Get(name); got != want {
						t.Errorf("request header %s = %q, want %q", name, got, want)
					}
				}

				reply := tt.reply
				if strings.Contains(reply, "%s") {
					reply = fmt.Sprintf(reply, acceptKey(key))
				}
				// The client may stop reading after a bad status line.
				_, _ = io.WriteString(server, reply)
			}()

			err := handshake(client, host, path, key)
			_ = client.Close() // unblocks the server if the client stopped reading early
			<-done

			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Errorf("handshake() error = %v, want error %t", err, tt.wantErr)
			}
		})
	}
}

func TestReadFrame(t *testing.T) {
	long := bytes.Repeat([]byte("ab"), 300)
	huge := bytes.Repeat([]byte("xyz"), 30000)

	tests := []struct {
		name  string
		input []byte
		want  frame
	}{
		{
			name:  "short text",
			input: []byte{0x81, 0x05, 'H', 'e', 'l', 'l', 'o'},
			want:  frame{fin: true, opcode: opText, payload: []byte("Hello")},
		},
		{
			name:  "not final",
			input: []byte{0x01, 0x03, 'H', 'e', 'l'},
			want:  frame{fin: false, opcode: opText, payload: []byte("Hel")},
		},
		{
			name:  "ping",
			input: []byte{0x89, 0x02, 'h', 'i'},
			want:  frame{fin: true, opcode: opPing, payload: []byte("hi")},
		},
		{
			name:  "16-bit length",
			input: append([]byte{0x81, 126, 0x02, 0x58}, long...),
			want:  frame{fin: true, opcode: opText, payload: long},
		},
		{
			name:  "64-bit length",
			input: append([]byte{0x81, 127, 0, 0, 0, 0, 0, 0x01, 0x5f, 0x90}, huge...),
			want:  frame{fin: true, opcode: opText, payload: huge},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readFrame(bytes.NewReader(tt.input), make([]byte, 0, 16))
			if err != nil {
				t.Fatalf("readFrame() error = %v", err)
			}
			if got.fin != tt.want.fin || got.opcode != tt.want.opcode || !bytes.Equal(got.payload, tt.want.payload) {
				t.Errorf("readFrame() = {fin:%t opcode:%#x len:%d}, want {fin:%t opcode:%#x len:%d}",
					got.fin, got.opcode, len(got.payload), tt.want.fin, tt.want.opcode, len(tt.want.payload))
			}
		})
	}
}

func TestReadFrameTruncated(t *testing.T) {
	for _, input := range [][]byte{
		{0x81},
		{0x81, 126, 0x01},
		{0x81, 127, 0, 0, 0},
		{0x81, 0x05, 'H', 'e'},
	} {
		_, err := readFrame(bytes.NewReader(input), nil)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("readFrame(% x) error = %v, want %v", input, err, io.ErrUnexpectedEOF)
		}
	}
}

func TestWriteFrame(t *testing.T) {
	tests := []struct {
		name    string
		opcode  byte
		size    int
		lenByte byte
	}{
		{"empty close", opClose, 0, 0},
		{"short text", opText, 125, 125},
		{"pong", opPong, 4, 4},
		{"16-bit length", opText, 126, 126},
		{"16-bit max", opText, 65535, 126},
		{"64-bit length", opText, 65536, 127},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := make([]byte, tt.size)
			for i := range payload {
				payload[i] = byte(i % 251)
			}

			var buf bytes.Buffer
			if err := writeFrame(&buf, tt.opcode, payload); err != nil {
				t.Fatalf("writeFrame() error = %v", err)
			}

			b := buf.Bytes()
			if want := 0x80 | tt.opcode; b[0] != want {
				t.Errorf("first byte = %#x, want %#x", b[0], want)
			}
			if b[1]&0x80 == 0 {
				t.Errorf("mask bit is not set")
			}
			if got := b[1] & 0x7F; got != tt.lenByte {
				t.Fatalf("length byte = %d, want %d", got, tt.lenByte)
			}

			b = b[2:]
			n := int(tt.lenByte)
			switch tt.lenByte {
			case 126:
				n = int(binary.BigEndian.Uint16(b))
				b = b[2:]
			case 127:
				n = int(binary.BigEndian.Uint64(b))
				b = b[8:]
			}
			if n != tt.size || len(b) != 4+n {
				t.Fatalf("encoded length %d with %d bytes left, want %d and %d", n, len(b), tt.size, 4+tt.size)
			}

			mask, data := b[:4], b[4:]
			for i := range data {
				data[i] ^= mask[i%4]
			}
			if !bytes.Equal(data, payload) {
				t.Errorf("unmasked payload differs from the input")
			}
		})
	}
}

func TestWriteFrameUnsupportedOpcode(t *testing.T) {
	var buf bytes.Buffer
	if err := writeFrame(&buf, opPing, []byte("hi")); err == nil {
		t.Error("writeFrame(opPing) error = nil, want an error")
	}
	if buf.Len() != 0 {
		t.Errorf("writeFrame(opPing) wrote %d bytes, want 0", buf.Len())
	}
}
