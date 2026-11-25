package main

import (
	"context"
	"net"
	"net/http"
	"time"
)

type partialConn struct {
	net.Conn
	partial time.Duration
}

func (c partialConn) Read(p []byte) (int, error) {
	if err := c.Conn.SetReadDeadline(time.Now().Add(c.partial)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func newHTTPClient(src net.IP, partial time.Duration) *http.Client {
	dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: src}, Timeout: partial}
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return partialConn{Conn: conn, partial: partial}, nil
		},
	}
	return &http.Client{Transport: transport}
}
