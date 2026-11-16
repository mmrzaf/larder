package testutil

import (
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// HTTPProxy is a minimal forwarding HTTP proxy with a request counter.
type HTTPProxy struct {
	Server    *httptest.Server
	transport *http.Transport
	count     atomic.Int64
}

// NewHTTPProxy starts an HTTP proxy.
func NewHTTPProxy(t testing.TB) *HTTPProxy {
	t.Helper()
	p := &HTTPProxy{transport: &http.Transport{Proxy: nil}}
	p.Server = httptest.NewServer(http.HandlerFunc(p.handle))
	t.Cleanup(func() {
		p.transport.CloseIdleConnections()
		p.Server.Close()
	})
	return p
}

// URL returns the proxy base URL.
func (p *HTTPProxy) URL() string { return p.Server.URL }

// Count returns the number of proxied requests.
func (p *HTTPProxy) Count() int64 { return p.count.Load() }

func (p *HTTPProxy) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.count.Add(1)
		p.tunnel(w, r)
		return
	}
	p.count.Add(1)
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Header = r.Header.Clone()
	out.Header.Del("Proxy-Connection")
	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		http.Error(w, "proxy error", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for name, values := range resp.Header {
		if isHopHeader(name) {
			continue
		}
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *HTTPProxy) tunnel(w http.ResponseWriter, r *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	target, err := net.Dial("tcp", r.Host)
	if err != nil {
		_, _ = client.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer target.Close()
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(target, client)
		_ = target.Close()
		close(done)
	}()
	_, _ = io.Copy(client, target)
	<-done
}

func isHopHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	}
	return false
}

// SOCKS5Proxy is a minimal SOCKS5 proxy with a request counter.
type SOCKS5Proxy struct {
	Addr  string
	count atomic.Int64

	ln net.Listener
	wg sync.WaitGroup
}

// NewSOCKS5Proxy starts a SOCKS5 proxy.
func NewSOCKS5Proxy(t testing.TB) *SOCKS5Proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &SOCKS5Proxy{Addr: ln.Addr().String(), ln: ln}
	go p.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		p.wg.Wait()
	})
	return p
}

// Count returns the number of proxied connections.
func (p *SOCKS5Proxy) Count() int64 { return p.count.Load() }

func (p *SOCKS5Proxy) serve() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handle(conn)
		}()
	}
}

func (p *SOCKS5Proxy) handle(client net.Conn) {
	defer client.Close()
	var header [2]byte
	if _, err := io.ReadFull(client, header[:]); err != nil || header[0] != 5 {
		return
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(client, methods); err != nil {
		return
	}
	if _, err := client.Write([]byte{5, 0}); err != nil {
		return
	}
	var request [4]byte
	if _, err := io.ReadFull(client, request[:]); err != nil || request[0] != 5 || request[1] != 1 {
		return
	}
	var host string
	switch request[3] {
	case 1:
		buf := make([]byte, 4)
		if _, err := io.ReadFull(client, buf); err != nil {
			return
		}
		host = net.IP(buf).String()
	case 3:
		var length [1]byte
		if _, err := io.ReadFull(client, length[:]); err != nil {
			return
		}
		buf := make([]byte, int(length[0]))
		if _, err := io.ReadFull(client, buf); err != nil {
			return
		}
		host = string(buf)
	case 4:
		buf := make([]byte, 16)
		if _, err := io.ReadFull(client, buf); err != nil {
			return
		}
		host = net.IP(buf).String()
	default:
		return
	}
	var portBuf [2]byte
	if _, err := io.ReadFull(client, portBuf[:]); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(portBuf[:])
	target, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		_, _ = client.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer target.Close()
	p.count.Add(1)
	if _, err := client.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(target, client)
		_ = target.Close()
		close(done)
	}()
	_, _ = io.Copy(client, target)
	<-done
}
