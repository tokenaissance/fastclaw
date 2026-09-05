package adapter

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth/domain"
)

// LoopbackCallbackReceiver listens on 127.0.0.1 for the provider redirect.
// The callback path is bound to a per-server callback ID (mix-up
// protection), so only the matching server's redirect is accepted.
type LoopbackCallbackReceiver struct {
	Port       int
	CallbackID string
}

// Listen starts the loopback server and returns the first callback.
func (r *LoopbackCallbackReceiver) Listen(ctx context.Context) (<-chan domain.CallbackParams, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", r.Port))
	if err != nil {
		return nil, err
	}
	ch := make(chan domain.CallbackParams, 1)
	go func() {
		defer ln.Close()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			req, err := http.ReadRequest(bufio.NewReader(conn))
			if err != nil {
				conn.Close()
				continue
			}
			expected := "/callback/" + r.CallbackID
			if r.CallbackID != "" && !strings.HasSuffix(req.URL.Path, expected) {
				_, _ = conn.Write([]byte("HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n"))
				conn.Close()
				continue
			}
			params, _ := domain.ParseCallbackURL(req.URL.String())
			_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<html><body>Authorization received. You can close this window.</body></html>"))
			conn.Close()
			ch <- params
			return
		}
	}()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	return ch, nil
}
