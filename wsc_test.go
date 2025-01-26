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
	"crypto/rand"
	"encoding/binary"
	"io"
	"testing"
)

func Test_write(t *testing.T) {
	tests := []struct {
		name    string
		opc     byte
		payload []byte
		wantErr bool
	}{
		{"simple text frame", 0x1, []byte("Hello"), false},
		{"binary frame", 0x2, []byte{0x01, 0x02, 0x03, 0x04}, false},
		{"medium payload frame", 0x1, make([]byte, 65535), false},
		{"large payload frame", 0x1, make([]byte, 65536), false},
		{"Empty payload frame", 0x1, []byte{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer

			err := write(&buf, tt.opc, tt.payload)
			if (err != nil) != tt.wantErr {
				t.Errorf("write() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				result := buf.Bytes()
				if len(result) < 2 {
					t.Fatalf("frame too short: %v", result)
				}

				finAndOpc := result[0]
				maskAndLen := result[1]

				if finAndOpc != (0x80 | tt.opc) {
					t.Errorf("unexpected FIN and opcode: got %v, want %v", finAndOpc, 0x80|tt.opc)
				}
				payloadLen := len(tt.payload)
				actualLen := int(maskAndLen & 0x7F)

				if payloadLen <= 125 {
					if actualLen != payloadLen {
						t.Errorf("unexpected payload length: got %v, want %v", actualLen, payloadLen)
					}
				} else if payloadLen <= 65535 {
					if actualLen != 126 {
						t.Errorf("unexpected payload length indicator: got %v, want %v", actualLen, 126)
					}
				} else {
					if actualLen != 127 {
						t.Errorf("unexpected payload length indicator: got %v, want %v", actualLen, 127)
					}
				}

				if maskAndLen&0x80 == 0 {
					t.Error("mask bit not set")
				}

				mask := result[len(result)-len(tt.payload)-4 : len(result)-len(tt.payload)]
				maskedPayload := result[len(result)-len(tt.payload):]

				for i, b := range tt.payload {
					expected := b ^ mask[i%4]
					if maskedPayload[i] != expected {
						t.Errorf("Unexpected masked payload at index %d: got %v, want %v", i, maskedPayload[i], expected)
					}
				}
			}
		})
	}
}

func Test_read(t *testing.T) {

	wsf := func(fin bool, opc byte, pln int) (io.Reader, []byte) {

		var buffer bytes.Buffer
		pld := make([]byte, pln)
		rand.Read(pld)

		firstByte := (1 << 7) | opc
		if !fin {
			firstByte = (0 << 7) | opc
		}

		buffer.WriteByte(byte(firstByte))

		if pln <= 125 {
			buffer.WriteByte(byte(pln))
		} else if pln <= 65535 {
			buffer.WriteByte(126)
			binary.Write(&buffer, binary.BigEndian, uint16(pln))
		} else {
			buffer.WriteByte(127)
			binary.Write(&buffer, binary.BigEndian, uint64(pln))
		}
		buffer.Write(pld)

		return &buffer, pld

	}

	t.Run("short frame", func(t *testing.T) {

		rdr, pld := wsf(true, 0x1, 125)

		if err := read(rdr, func(fin bool, opc byte, msg []byte) {
			if !fin {
				t.Errorf("read() got fin %v, want true", fin)
			}
			if opc != 0x1 {
				t.Errorf("read() got opc %v, want 1", opc)
			}
			if !bytes.Equal(pld, msg) {
				t.Error("the message read does not match the one sent")
			}

		}); err != nil && err != io.EOF {
			t.Errorf("read() error %v got <nil>", err)
		}
	})
	t.Run("medium frame", func(t *testing.T) {

		rdr, pld := wsf(true, 0x1, 65535)

		if err := read(rdr, func(fin bool, opc byte, msg []byte) {
			if !fin {
				t.Errorf("read() got fin %v, want true", fin)
			}
			if opc != 0x1 {
				t.Errorf("read() got opc %v, want 1", opc)
			}
			if !bytes.Equal(pld, msg) {
				t.Error("the message read does not match the one sent")
			}

		}); err != nil && err != io.EOF {
			t.Errorf("read() error %v got <nil>", err)
		}
	})
	t.Run("long frame", func(t *testing.T) {

		rdr, pld := wsf(true, 0x1, 65536)

		if err := read(rdr, func(fin bool, opc byte, msg []byte) {

			if !fin {
				t.Errorf("read() got fin %v, want true", fin)
			}
			if opc != 0x1 {
				t.Errorf("read() got opc %v, want 1", opc)
			}
			if !bytes.Equal(pld, msg) {
				t.Error("the message read does not match the one sent")
			}

		}); err != nil && err != io.EOF {
			t.Errorf("read() error %v got <nil>", err)
		}
	})

	t.Run("continue frame", func(t *testing.T) {

		rdr, pld := wsf(false, 0x0, 125)

		if err := read(rdr, func(fin bool, opc byte, msg []byte) {
			if fin {
				t.Errorf("read() got fin %v, want false", fin)
			}
			if opc != 0x0 {
				t.Errorf("read() got opc %v, want 0", opc)
			}
			if !bytes.Equal(pld, msg) {
				t.Error("the message read does not match the one sent")
			}

		}); err != nil && err != io.EOF {
			t.Errorf("read() error %v got <nil>", err)
		}
	})
	t.Run("binary frame", func(t *testing.T) {

		rdr, pld := wsf(true, 0x2, 125)

		if err := read(rdr, func(fin bool, opc byte, msg []byte) {
			if !fin {
				t.Errorf("read() got fin %v, want true", fin)
			}
			if opc != 0x2 {
				t.Errorf("read() got opc %v, want 2", opc)
			}
			if !bytes.Equal(pld, msg) {
				t.Error("the message read does not match the one sent")
			}

		}); err != nil && err != io.EOF {
			t.Errorf("read() error %v got <nil>", err)
		}
	})
	t.Run("ping frame", func(t *testing.T) {

		rdr, pld := wsf(true, 0x9, 125)

		if err := read(rdr, func(fin bool, opc byte, msg []byte) {
			if !fin {
				t.Errorf("read() got fin %v, want true", fin)
			}
			if opc != 0x9 {
				t.Errorf("read() got opc %v, want 9", opc)
			}
			if !bytes.Equal(pld, msg) {
				t.Error("the message read does not match the one sent")
			}

		}); err != nil && err != io.EOF {
			t.Errorf("read() error %v got <nil>", err)
		}
	})
	t.Run("pong frame", func(t *testing.T) {

		rdr, pld := wsf(true, 0xA, 125)

		if err := read(rdr, func(fin bool, opc byte, msg []byte) {
			if !fin {
				t.Errorf("read() got fin %v, want true", fin)
			}
			if opc != 0xA {
				t.Errorf("read() got opc %v, want 10", opc)
			}
			if !bytes.Equal(pld, msg) {
				t.Error("the message read does not match the one sent")
			}

		}); err != nil && err != io.EOF {
			t.Errorf("read() error %v got <nil>", err)
		}
	})
	t.Run("close frame", func(t *testing.T) {

		rdr, pld := wsf(true, 0x8, 125)

		if err := read(rdr, func(fin bool, opc byte, msg []byte) {
			if !fin {
				t.Errorf("read() got fin %v, want true", fin)
			}
			if opc != 0x8 {
				t.Errorf("read() got opc %v, want 8", opc)
			}
			if !bytes.Equal(pld, msg) {
				t.Error("the message read does not match the one sent")
			}

		}); err != nil && err != io.EOF {
			t.Errorf("read() error %v got <nil>", err)
		}
	})
	t.Run("incoming handler is nil", func(t *testing.T) {
		rdr, _ := wsf(true, 0x8, 125)
		if err := read(rdr, nil); err == nil && err != io.EOF {
			t.Errorf("read() error %v got not nil", err)
		}
	})
}

func Test_parse(t *testing.T) {
	tests := []struct {
		name string
		url  string
		host string
		port string
		path string
		err  bool
	}{
		{"ip address", "127.0.0.1", "127.0.0.1", "443", "/", false},
		{"ip address with path", "127.0.0.1/path/path/", "127.0.0.1", "443", "/path/path", false},
		{"ip address with path and port", "127.0.0.1:9443", "127.0.0.1", "9443", "/", false},

		{"https host", "https://www.host.com", "host.com", "443", "/", false},
		{"https host with path", "https://www.host.com/path/path/", "host.com", "443", "/path/path", false},
		{"https host with path and port", "https://www.host.com:9443", "host.com", "9443", "/", false},

		{"http host", "https://www.host.com", "host.com", "443", "/", false},
		{"http host with path", "https://www.host.com/path/path/", "host.com", "443", "/path/path", false},
		{"http host with path and port", "https://www.host.com:9443", "host.com", "9443", "/", false},

		{"wss host", "https://www.host.com", "host.com", "443", "/", false},
		{"wss host with path", "https://www.host.com/path/path/", "host.com", "443", "/path/path", false},
		{"wss host with path and port", "https://www.host.com:9443", "host.com", "9443", "/", false},

		{"ws host", "https://www.host.com", "host.com", "443", "/", false},
		{"ws host with path", "https://www.host.com/path/path/", "host.com", "443", "/path/path", false},
		{"ws host with path and port", "https://www.host.com:9443", "host.com", "9443", "/", false},

		{"incorrect scheme", "incorrect://www.host.com", "host.com", "443", "/", false},
		{"error - url not specified", "", "", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port, path, err := parse(tt.url)
			if (err != nil) != tt.err {
				t.Errorf("wsc.url() error = %v, err %v", err, tt.err)
				return
			}
			if host != tt.host {
				t.Errorf("wsc.url() host got = %v, want %v", host, tt.host)
			}
			if port != tt.port {
				t.Errorf("wsc.url() port got = %v, want %v", port, tt.port)
			}
			if path != tt.path {
				t.Errorf("wsc.url() path got = %v, want %v", path, tt.path)
			}
		})
	}
}
