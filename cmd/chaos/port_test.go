//go:build integration

package main

import "net"

// reservedPort is a port that was bound and immediately released.
//
// There is a race in principle between releasing and rebinding; in practice the
// window is microseconds. The alternative -- a node constructor that binds in
// one call and serves in another -- is production complexity added to serve a
// test, which is the wrong direction.
type reservedPort struct {
	port int
	l    net.Listener
}

func netListen() (*reservedPort, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		l.Close()
		return nil, net.UnknownNetworkError("not a tcp address")
	}
	return &reservedPort{port: addr.Port, l: l}, nil
}

func (r *reservedPort) close() error { return r.l.Close() }
