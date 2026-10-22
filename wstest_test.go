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
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The test server below is the server side of RFC 6455, written separately
// from the client code so that a mistake in one shows up as a mismatch
// instead of hiding on both ends of a test.

// testTimeout bounds every read and write in these tests, so a protocol
// mismatch fails the test instead of hanging it.
const testTimeout = 5 * time.Second

// serverAccept computes Sec-WebSocket-Accept the way a server does (RFC 6455
// §4.2.2). The GUID is spelled out instead of using acceptGUID on purpose.
func serverAccept(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// hasToken reports whether the comma-separated header name contains token,
// ignoring case, as §4.2.1 requires for Upgrade and Connection.
func hasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for t := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// newTestServer starts a server that answers a valid opening handshake on any
// path with 101 and then passes the request and the hijacked connection to
// handle. Anything else gets 400 Bad Request. The connection is closed when
// handle returns, and the test does not finish until every handle call has.
func newTestServer(t *testing.T, handle func(r *http.Request, rw *bufio.ReadWriter)) *httptest.Server {
	t.Helper()

	var handlers sync.WaitGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Sec-WebSocket-Key")
		nonce, err := base64.StdEncoding.DecodeString(key)
		if r.Method != http.MethodGet ||
			!hasToken(r.Header, "Upgrade", "websocket") ||
			!hasToken(r.Header, "Connection", "Upgrade") ||
			r.Header.Get("Sec-WebSocket-Version") != "13" ||
			err != nil || len(nonce) != 16 {
			http.Error(w, "not a WebSocket handshake", http.StatusBadRequest)
			return
		}

		// Hijacked connections are not tracked by httptest, so wait for
		// them separately.
		handlers.Add(1)
		defer handlers.Done()

		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if err := conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
			t.Errorf("set deadline: %v", err)
			return
		}

		_, err = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", serverAccept(key))
		if err == nil {
			err = rw.Flush()
		}
		if err != nil {
			t.Errorf("write 101 response: %v", err)
			return
		}
		handle(r, rw)
	}))
	t.Cleanup(func() {
		srv.Close()
		handlers.Wait()
	})
	return srv
}

// dialTestServer opens a TCP connection to srv and runs the client handshake
// for path on it. The connection is closed when the test ends.
func dialTestServer(t *testing.T, srv *httptest.Server, path string) net.Conn {
	t.Helper()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}
	if err := handshake(conn, srv.Listener.Addr().String(), path, "dGhlIHNhbXBsZSBub25jZQ=="); err != nil {
		t.Fatalf("handshake() error = %v", err)
	}
	return conn
}

// readClientFrame reads one frame the way a server does: the frame must be
// masked (§5.3), and the payload comes back unmasked.
func readClientFrame(r io.Reader) (frame, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return frame{}, err
	}
	if hdr[1]&0x80 == 0 {
		return frame{}, errors.New("client frame is not masked")
	}

	n := uint64(hdr[1] & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return frame{}, err
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return frame{}, err
		}
		n = binary.BigEndian.Uint64(ext[:])
	}

	var mask [4]byte
	if _, err := io.ReadFull(r, mask[:]); err != nil {
		return frame{}, err
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return frame{}, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return frame{fin: hdr[0]&0x80 != 0, opcode: hdr[0] & 0x0F, payload: payload}, nil
}

// writeServerFrame writes payload as one final unmasked frame, using the
// shortest length encoding as §5.2 requires.
func writeServerFrame(w io.Writer, opcode byte, payload []byte) error {
	b := []byte{0x80 | opcode}
	switch n := len(payload); {
	case n <= 125:
		b = append(b, byte(n))
	case n <= 0xFFFF:
		b = append(b, 126)
		b = binary.BigEndian.AppendUint16(b, uint16(n))
	default:
		b = append(b, 127)
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	_, err := w.Write(append(b, payload...))
	return err
}

func TestServerAccept(t *testing.T) {
	// Example from RFC 6455 §1.3.
	const key, want = "dGhlIHNhbXBsZSBub25jZQ==", "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	if got := serverAccept(key); got != want {
		t.Errorf("serverAccept(%q) = %q, want %q", key, got, want)
	}
}

func TestTestServerRejectsBadHandshake(t *testing.T) {
	srv := newTestServer(t, func(*http.Request, *bufio.ReadWriter) {
		t.Error("handle called for a bad handshake")
	})

	const good = "Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	tests := []struct {
		name    string
		method  string
		headers string
	}{
		{"post", "POST", good},
		{"no upgrade", "GET", strings.Replace(good, "Upgrade: websocket\r\n", "", 1)},
		{"no connection upgrade", "GET", strings.Replace(good, "Connection: Upgrade", "Connection: keep-alive", 1)},
		{"version 8", "GET", strings.Replace(good, "Version: 13", "Version: 8", 1)},
		{"short key", "GET", strings.Replace(good, "dGhlIHNhbXBsZSBub25jZQ==", "c2hvcnQ=", 1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := net.Dial("tcp", srv.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if err := conn.SetDeadline(time.Now().Add(testTimeout)); err != nil {
				t.Fatal(err)
			}

			if _, err := fmt.Fprintf(conn, "%s / HTTP/1.1\r\nHost: example.com\r\nContent-Length: 0\r\n%s\r\n", tt.method, tt.headers); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
			}
		})
	}
}

func TestHandshakeWithTestServer(t *testing.T) {
	const path = "/stream?id=7&lang=en"

	requests := make(chan *http.Request, 1)
	srv := newTestServer(t, func(r *http.Request, _ *bufio.ReadWriter) {
		requests <- r
	})
	dialTestServer(t, srv, path)

	r := <-requests
	if want := srv.Listener.Addr().String(); r.Host != want {
		t.Errorf("Host = %q, want %q", r.Host, want)
	}
	if got := r.URL.RequestURI(); got != path {
		t.Errorf("request URI = %q, want %q", got, path)
	}
}

// The frame vectors from RFC 6455 §5.7. Binary data in the length examples is
// left out of the RFC, so the tests fill it in.
var (
	vecText     = []byte{0x81, 0x05, 0x48, 0x65, 0x6c, 0x6c, 0x6f}
	vecMasked   = []byte{0x81, 0x85, 0x37, 0xfa, 0x21, 0x3d, 0x7f, 0x9f, 0x4d, 0x51, 0x58}
	vecFirst    = []byte{0x01, 0x03, 0x48, 0x65, 0x6c}
	vecLast     = []byte{0x80, 0x02, 0x6c, 0x6f}
	vecPing     = []byte{0x89, 0x05, 0x48, 0x65, 0x6c, 0x6c, 0x6f}
	vecPong     = []byte{0x8a, 0x85, 0x37, 0xfa, 0x21, 0x3d, 0x7f, 0x9f, 0x4d, 0x51, 0x58}
	vec256Head  = []byte{0x82, 0x7E, 0x01, 0x00}
	vec64KiHead = []byte{0x82, 0x7F, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00}
)

func filler(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func TestReadFrameRFCVectors(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  frame
	}{
		{"unmasked text", vecText, frame{fin: true, opcode: opText, payload: []byte("Hello")}},
		{"first fragment", vecFirst, frame{fin: false, opcode: opText, payload: []byte("Hel")}},
		{"last fragment", vecLast, frame{fin: true, opcode: 0x0, payload: []byte("lo")}},
		{"unmasked ping", vecPing, frame{fin: true, opcode: opPing, payload: []byte("Hello")}},
		{"256 bytes", append(bytes.Clone(vec256Head), filler(256)...), frame{fin: true, opcode: 0x2, payload: filler(256)}},
		{"64 KiB", append(bytes.Clone(vec64KiHead), filler(65536)...), frame{fin: true, opcode: 0x2, payload: filler(65536)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readFrame(bytes.NewReader(tt.input), nil)
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

// The server helpers are checked against the same vectors, so the
// end-to-end tests below can rely on them.
func TestTestServerFrameVectors(t *testing.T) {
	for _, tt := range []struct {
		name   string
		input  []byte
		opcode byte
	}{
		{"masked text", vecMasked, opText},
		{"masked pong", vecPong, opPong},
	} {
		got, err := readClientFrame(bytes.NewReader(tt.input))
		if err != nil {
			t.Fatalf("%s: readClientFrame() error = %v", tt.name, err)
		}
		if !got.fin || got.opcode != tt.opcode || string(got.payload) != "Hello" {
			t.Errorf("%s: readClientFrame() = {fin:%t opcode:%#x payload:%q}, want {fin:true opcode:%#x payload:\"Hello\"}",
				tt.name, got.fin, got.opcode, got.payload, tt.opcode)
		}
	}
	if _, err := readClientFrame(bytes.NewReader(vecText)); err == nil {
		t.Error("readClientFrame(unmasked) error = nil, want an error")
	}

	for _, tt := range []struct {
		name    string
		opcode  byte
		payload []byte
		want    []byte
	}{
		{"text", opText, []byte("Hello"), vecText},
		{"ping", opPing, []byte("Hello"), vecPing},
		{"256 bytes", 0x2, filler(256), append(bytes.Clone(vec256Head), filler(256)...)},
		{"64 KiB", 0x2, filler(65536), append(bytes.Clone(vec64KiHead), filler(65536)...)},
	} {
		var buf bytes.Buffer
		if err := writeServerFrame(&buf, tt.opcode, tt.payload); err != nil {
			t.Fatalf("%s: writeServerFrame() error = %v", tt.name, err)
		}
		if !bytes.Equal(buf.Bytes(), tt.want) {
			t.Errorf("%s: writeServerFrame() = % x..., want % x...", tt.name, buf.Bytes()[:min(buf.Len(), 12)], tt.want[:min(len(tt.want), 12)])
		}
	}
}

// TestEchoThroughTestServer sends text frames of every length encoding to an
// echo server. The server only answers after it has read a frame, so nothing
// arrives while the client is still reading the handshake response.
func TestEchoThroughTestServer(t *testing.T) {
	srv := newTestServer(t, func(_ *http.Request, rw *bufio.ReadWriter) {
		for {
			f, err := readClientFrame(rw)
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				t.Errorf("server read: %v", err)
				return
			}
			if err := writeServerFrame(rw, f.opcode, f.payload); err != nil {
				t.Errorf("server write: %v", err)
				return
			}
			if err := rw.Flush(); err != nil {
				t.Errorf("server flush: %v", err)
				return
			}
		}
	})
	conn := dialTestServer(t, srv, "/echo")

	buf := make([]byte, 0, 64)
	for _, n := range []int{0, 1, 125, 126, 65535, 65536, 70000} {
		payload := filler(n)
		if err := writeFrame(conn, opText, payload); err != nil {
			t.Fatalf("writeFrame(%d bytes) error = %v", n, err)
		}
		got, err := readFrame(conn, buf)
		if err != nil {
			t.Fatalf("readFrame() after %d bytes error = %v", n, err)
		}
		if !got.fin || got.opcode != opText || !bytes.Equal(got.payload, payload) {
			t.Errorf("echo of %d bytes = {fin:%t opcode:%#x len:%d}, want {fin:true opcode:%#x len:%d}",
				n, got.fin, got.opcode, len(got.payload), opText, n)
		}
		buf = got.payload
	}
}
