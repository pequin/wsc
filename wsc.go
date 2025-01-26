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
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"strings"
	"time"
)

type message struct {
	fin bool
	opc byte
	pln []byte
}

// Creates a new web socket connection.
func dial(url string) (*tls.Conn, error) {

	key := make([]byte, 16)
	io.ReadFull(rand.Reader, key)
	wsk := base64.StdEncoding.EncodeToString(key)

	host, port, path, err := parse(url)
	if err != nil {
		return nil, err
	}

	con, err := tls.Dial("tcp", fmt.Sprintf("%s:%s", host, port), &tls.Config{
		InsecureSkipVerify: false,
	})

	if err != nil {
		return nil, err
	}

	if err := con.Handshake(); err != nil {
		defer con.Close()
		return nil, err
	}

	if err := con.SetDeadline(time.Now().Add(time.Minute)); err != nil {
		defer con.Close()
		return nil, err
	}

	if _, err := con.Write([]byte(fmt.Sprintf("GET %s HTTP/1.1\r\n"+
		"Host: %s\r\nUpgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\n"+
		"Sec-WebSocket-Key: %s\r\n\r\n",

		path,
		host,
		wsk))); err != nil {
		defer con.Close()
		return nil, err
	}

	buf := bufio.NewReader(con)

	// Reqad start-line.
	if str, err := buf.ReadString('\n'); err != nil {
		defer con.Close()
		return nil, err
	} else if strings.Contains(str, "404 Not Found") {
		defer con.Close()
		return nil, fmt.Errorf("path %s is not found", host)
	} else if !strings.Contains(str, "101 Switching") {
		defer con.Close()
		return nil, errors.New("the server did not switch to the websocket protocol")
	}

	swa := false // Success sec websocket accept.
	for {
		str, err := buf.ReadString('\r')
		if err != nil {
			defer con.Close()
			return nil, err
		}

		str = strings.ReplaceAll(strings.ReplaceAll(str, "\n", ""), "\r", "")
		if len(str) == 0 {
			break
		}

		hed := strings.SplitN(str, ": ", 2)

		if len(hed) != 2 {
			defer con.Close()
			return nil, errors.New("the server did not switch to the websocket protocol")
		}

		if strings.ToLower(hed[0]) == "sec-websocket-accept" {
			h := sha1.New()
			h.Write([]byte(wsk))
			h.Write([]byte("258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
			ack := base64.StdEncoding.EncodeToString(h.Sum(nil))

			if !bytes.Equal([]byte(hed[1]), []byte(ack)) {
				defer con.Close()
				return nil, fmt.Errorf("invalid Sec-WebSocket-Accept: expected %s, got %s", wsk, ack)
			}

			swa = true
		}
	}

	if !swa {
		defer con.Close()
		return nil, errors.New("the server response is missing a header")
	}

	err = con.SetDeadline(time.Time{})
	if err != nil {
		defer con.Close()
		return nil, err
	}

	return con, nil
}

func write(w io.Writer, opc byte, pld []byte) error {
	fme := []byte{0, 0}
	msk := []byte{0, 0, 0, 0}
	pln := len(pld)
	var err error

	fme[0] = (1 << 7) | byte(opc)
	if pln <= 125 {
		fme[1] = byte(0x80) | byte(pln)
	} else if pln <= 65535 {
		fme[1] = byte(0x80) | byte(126)
		fme = append(fme, byte(pln>>8), byte(pln))
	} else {
		fme[1] = byte(0x80) | byte(127) // Mask = 1, len = 127.
		fme = append(fme,
			byte(pln>>56), byte(pln>>48),
			byte(pln>>40), byte(pln>>32),
			byte(pln>>24), byte(pln>>16),
			byte(pln>>8), byte(pln))
	}

	if _, err = rand.Read(msk); err != nil {
		log.Fatalln(err)
	}

	fme = append(fme, msk...)

	for i, b := range pld {
		fme = append(fme, b^msk[i%4])
	}

	if _, err = w.Write(fme); err != nil {
		return err
	}

	return nil
}

func read(r io.Reader, f func(fin bool, opc byte, msg []byte)) error {

	var fin bool
	var opc byte
	var pln int
	var err error
	by2 := make([]byte, 2)
	by8 := make([]byte, 8)

	for {
		_, err = io.ReadFull(r, by2)
		if err != nil {
			return err
		}

		fin = by2[0]&0x80 != 0
		opc = by2[0] & 0x0F
		pln = int(by2[1] & 0x7F)

		if pln == 126 {
			_, err = io.ReadFull(r, by2)
			if err != nil {
				return err
			}
			pln = int(binary.BigEndian.Uint16(by2))

		} else if pln == 127 {
			_, err = io.ReadFull(r, by8)
			if err != nil {
				return err
			}
			pln = int(binary.BigEndian.Uint64(by8))
		}

		pld := make([]byte, 0, pln)

		_, err = io.ReadFull(r, pld[:pln])
		if err != nil {
			return err
		}

		if f == nil {
			return errors.New("error read: incoming message handler is nil")
		}

		if pln > 0 {
			f(fin, opc, pld[:pln])
		}
	}
}

// Returns the components of the URL host, port, path.
func parse(url string) (string, string, string, error) {

	url = regexp.MustCompile(`^[a-zA-Z]+://`).ReplaceAllString(url, "")

	if len(url) == 0 {
		return "", "", "", errors.New("url parsing failed: hostname cannot be empty")
	}

	var host string
	var port string
	var path string

	hostAndPath := strings.SplitN(
		strings.TrimSuffix(
			strings.NewReplacer(
				"https://", "",
				"http://", "",
				"wss://", "",
				"ws://", "",
				"www.", "",
			).Replace(url),
			"/"),
		"/", 2)

	hostAndPort := strings.SplitN(hostAndPath[0], ":", 2)

	host = hostAndPort[0]

	if len(hostAndPort) == 2 {
		port = hostAndPort[1]
	} else {
		port = "443"
	}

	if len(hostAndPath) == 2 {
		path = fmt.Sprintf("/%s", hostAndPath[1])
	} else {
		path = "/"
	}

	return host, port, path, nil
}
