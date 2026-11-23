package execrunner

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const allowedConnectTarget = "github.com:443"

var proxyDialContext = (&net.Dialer{Timeout: 10 * time.Second}).DialContext

// connectProxy is deliberately not a general HTTP proxy. It accepts one
// exact HTTPS CONNECT authority and never inspects or logs tunnel payloads.
type connectProxy struct {
	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	closed   sync.Once
	mu       sync.Mutex
	clients  map[net.Conn]struct{}
	closing  bool
	wg       sync.WaitGroup
}

func startConnectProxy() (*connectProxy, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &connectProxy{
		listener: l,
		ctx:      ctx,
		cancel:   cancel,
		clients:  make(map[net.Conn]struct{}),
	}
	p.wg.Add(1)
	go p.serve()
	return p, nil
}

func (p *connectProxy) Addr() string { return p.listener.Addr().String() }

func (p *connectProxy) Close() error {
	var err error
	p.closed.Do(func() {
		p.cancel()
		err = p.listener.Close()
		p.mu.Lock()
		p.closing = true
		for client := range p.clients {
			_ = client.Close()
		}
		p.mu.Unlock()
	})
	p.wg.Wait()
	return err
}

func (p *connectProxy) serve() {
	defer p.wg.Done()
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Print("git egress proxy: accept_failed")
			}
			return
		}
		p.mu.Lock()
		if p.closing {
			p.mu.Unlock()
			_ = conn.Close()
			continue
		}
		p.clients[conn] = struct{}{}
		p.wg.Add(1)
		p.mu.Unlock()
		go func() {
			defer func() {
				p.mu.Lock()
				delete(p.clients, conn)
				p.mu.Unlock()
				p.wg.Done()
			}()
			p.handle(conn)
		}()
	}
}

func (p *connectProxy) handle(client net.Conn) {
	defer client.Close()
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	req, err := http.ReadRequest(bufio.NewReader(io.LimitReader(client, 16<<10)))
	if err != nil {
		log.Print("git egress proxy: malformed_request")
		writeProxyResponse(client, http.StatusBadRequest)
		return
	}
	if req.Body != nil {
		_ = req.Body.Close()
	}
	if req.Method != http.MethodConnect || req.Host != allowedConnectTarget || req.RequestURI != allowedConnectTarget {
		log.Print("git egress proxy: request_denied")
		writeProxyResponse(client, http.StatusForbidden)
		return
	}
	upstream, err := proxyDialContext(p.ctx, "tcp", allowedConnectTarget)
	if err != nil {
		log.Print("git egress proxy: upstream_unavailable")
		writeProxyResponse(client, http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	_ = client.SetReadDeadline(time.Time{})
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

func writeProxyResponse(conn net.Conn, status int) {
	_, _ = io.WriteString(conn, "HTTP/1.1 "+strconv.Itoa(status)+" "+http.StatusText(status)+"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
}
