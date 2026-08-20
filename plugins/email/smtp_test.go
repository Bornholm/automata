package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
)

// startSMTPServer lance un mini serveur SMTP qui capture les messages.
// Assez de protocole pour gomail sans TLS ni authentification.
func startSMTPServer(t *testing.T) (host string, port int, received func() []string) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen smtp: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var mu sync.Mutex
	var messages []string

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				r := bufio.NewReader(conn)
				w := bufio.NewWriter(conn)
				say := func(line string) { fmt.Fprintf(w, "%s\r\n", line); _ = w.Flush() }

				say("220 test SMTP")
				var data strings.Builder
				inData := false
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimRight(line, "\r\n")

					if inData {
						if line == "." {
							inData = false
							mu.Lock()
							messages = append(messages, data.String())
							mu.Unlock()
							data.Reset()
							say("250 OK")
							continue
						}
						data.WriteString(line + "\n")
						continue
					}

					switch {
					case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
						say("250 test")
					case strings.HasPrefix(line, "MAIL FROM"), strings.HasPrefix(line, "RCPT TO"):
						say("250 OK")
					case line == "DATA":
						inData = true
						say("354 go")
					case line == "QUIT":
						say("221 bye")
						return
					default:
						say("250 OK")
					}
				}
			}(conn)
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), messages...)
	}
}
