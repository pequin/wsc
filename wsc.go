package wsc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"net"
	"net/url"
	"strings"
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

func dial(ctx context.Context, URL string, request <-chan []byte, response chan<- []byte, stream chan<- []byte) error {

	var conn *tls.Conn
	var host string
	var path string
	var port string

	var err error

	if url, err := url.Parse(URL); err == nil {

		host, _ = strings.CutPrefix(url.Host, "www.")
		path = url.RequestURI()
		port = url.Port()

		if port == "" {
			port = "443"
		}

	} else {
		return err
	}

	key := make([]byte, 16)
	io.ReadFull(rand.Reader, key)
	wsk := base64.StdEncoding.EncodeToString(key)
	key = nil

	if conn, err = tls.Dial("tcp", fmt.Sprintf("%s:%s", host, port), &tls.Config{
		InsecureSkipVerify: false,
	}); err != nil {
		return err
	}

	if err = conn.Handshake(); err != nil {
		return err
	}

	if err = conn.SetDeadline(time.Now().Add(time.Second * 10)); err != nil {
		return err
	}

	if _, err = conn.Write([]byte(fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n", path, host, wsk))); err != nil {
		return err
	}

	// Read start-line.
	buf := bufio.NewReader(conn)
	if str, err := buf.ReadString('\n'); err != nil {
		return err
	} else if strings.Contains(str, "404 Not Found") {
		return fmt.Errorf("%s: the path %s to is wrong", host, path)
	} else if !strings.Contains(str, "101 Switching") {
		return fmt.Errorf("%s: the server did not switch to the websocket protocol", host)
	}

	// Reading further lines.
	var str string
	var sts []string
	var hsh hash.Hash
	for {

		if str, err = buf.ReadString('\r'); err != nil {
			return err
		}

		str = strings.ReplaceAll(strings.ReplaceAll(str, "\n", ""), "\r", "")

		// The last line has been read.
		if len(str) == 0 {
			break
		}

		sts = strings.SplitN(str, ": ", 2)

		if len(sts) != 2 {
			return fmt.Errorf("%s: the server did not switch to the websocket protocol", host)
		}

		if strings.ToLower(sts[0]) == "sec-websocket-accept" {

			hsh = sha1.New()
			hsh.Write([]byte(wsk))
			hsh.Write([]byte("258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
			str = base64.StdEncoding.EncodeToString(hsh.Sum(nil))

			if !bytes.Equal([]byte(sts[1]), []byte(str)) {
				return fmt.Errorf("%s: invalid Sec-WebSocket-Accept", host)
			}
		}
	}

	if err = conn.SetDeadline(time.Time{}); err != nil {
		return err
	}

	host = ""
	path = ""
	port = ""
	wsk = ""
	str = ""
	sts = nil
	hsh = nil

	wait := make(chan error)
	reader := struct {
		readed chan []byte
		wait   chan error
	}{
		readed: make(chan []byte),
		wait:   make(chan error),
	}

	writer := struct {
		write chan struct {
			opc byte
			pln []byte
		}
		writted chan struct{}
		wait    chan error
	}{
		write: make(chan struct {
			opc byte
			pln []byte
		}),
		writted: make(chan struct{}),
		wait:    make(chan error),
	}

	// Reader.
	go func() {
		var fin bool
		var opc byte
		var pln int
		var err error

		by2 := make([]byte, 2)
		by8 := make([]byte, 8)
		pld := make([]byte, 0, 256)
	done:
		for {
			if _, err = io.ReadFull(conn, by2); err != nil {
				break done
			}

			fin = by2[0]&0x80 != 0
			opc = by2[0] & 0x0F
			pln = int(by2[1] & 0x7F)

			if pln == 126 {
				_, err = io.ReadFull(conn, by2)
				if err != nil {
					break done
				}
				pln = int(binary.BigEndian.Uint16(by2))

			} else if pln == 127 {
				_, err = io.ReadFull(conn, by8)
				if err != nil {
					break done
				}
				pln = int(binary.BigEndian.Uint64(by8))
			}

			if cap(pld) < pln {
				pld = make([]byte, 0, pln)
			}

			if _, err = io.ReadFull(conn, pld[:pln]); err != nil {
				break done
			}

			switch opc {
			case 0x1: // Text

				reader.readed <- pld[:pln]

			case 0x8: // Close
				err = errors.New("the server closed the connection")
				break done
			case 0x9: // ping

				writer.write <- struct {
					opc byte
					pln []byte
				}{
					opc: 0xA,
					pln: pld[:pln],
				}
				<-writer.writted

			default:
				err = errors.New("frame type not supported by client")
				break done
			}

			if !fin {
				err = errors.New("the final frame was not received")
				break done
			}

		}

		defer func() {
			reader.wait <- err
		}()

	}()

	// Writer.
	go func() {

		var err error
		fme := make([]byte, 2)
		msk := make([]byte, 4)
	done:
		for w := range writer.write {

			if w.opc != 0x1 && w.opc != 0x8 && w.opc != 0xA {
				err = errors.New("frame type not supported by client")
				break done
			}

			fme[0] = (1 << 7) | byte(w.opc)
			if l := len(w.pln); l <= 125 {
				fme[1] = byte(0x80) | byte(len(w.pln))
			} else if l <= 65535 {
				fme[1] = byte(0x80) | byte(126)
				fme = append(fme, byte(l>>8), byte(l))
			} else {
				fme[1] = byte(0x80) | byte(127) // Mask = 1, len = 127.
				fme = append(fme,
					byte(l>>56), byte(l>>48),
					byte(l>>40), byte(l>>32),
					byte(l>>24), byte(l>>16),
					byte(l>>8), byte(l))
			}

			if _, err = rand.Read(msk); err != nil {
				break done
			}

			fme = append(fme, msk...)

			for i, b := range w.pln {
				fme = append(fme, b^msk[i%4])
			}

			if _, err = conn.Write(fme); err != nil {
				break done
			}

			// Closing code.
			if w.opc == 0x8 {
				break done
			}

			fme = fme[:2]

			writer.writted <- struct{}{}

		}

		defer func() {
			writer.wait <- err
		}()

	}()

	// Input - output.
	var iow sync.WaitGroup
	iow.Add(1)
	go func() {
		var pln []byte
	done:
		for {
			select {
			case pln = <-reader.readed:

				stream <- pln

			case pln = <-request:

				writer.write <- struct {
					opc byte
					pln []byte
				}{
					opc: 0x1,
					pln: pln,
				}

				<-writer.writted

				response <- <-reader.readed

			case <-ctx.Done():
				break done
			}
		}

		iow.Done()

	}()

	// Pulse.
	go func() {

		var err error
	done:
		for {

			select {

			case <-ctx.Done():

				// Waiting for input - output.
				iow.Wait()

				writer.write <- struct {
					opc byte
					pln []byte
				}{
					opc: 0x8,
					pln: nil,
				}

				// Waiting for the writer to done.
				if err := <-writer.wait; err != nil {
					log.Fatalln(err)
				}

				if err := conn.SetDeadline(time.Now()); err != nil {
					log.Fatalln(err)
				}

				// Waiting for the reader to done.
				err = <-reader.wait
				if err, ok := err.(net.Error); !ok || !err.Timeout() {
					log.Fatalln(err)
				}

				err = nil

				break done

			case err = <-writer.wait:
				log.Fatalln(err)
			case err = <-reader.wait:
				log.Fatalln(err)
			}
		}

		defer func() {
			wait <- err
		}()
	}()

	err = <-wait
	if err != nil {
		return err
	}

	if err = conn.Close(); err != nil {
		return err
	}

	close(wait)
	close(reader.readed)
	close(reader.wait)
	close(writer.write)
	close(writer.writted)
	close(writer.wait)

	return err
}
