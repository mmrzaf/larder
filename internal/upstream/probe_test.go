package upstream

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/config"
)

func testTimeouts() config.Timeouts {
	t := config.Defaults().Timeouts
	t.Connect = config.Duration(2 * time.Second)
	t.TLSHandshake = config.Duration(2 * time.Second)
	t.ResponseHeader = config.Duration(2 * time.Second)
	return t
}

func TestProbeDirect(t *testing.T) {
	var userAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userAgent = r.UserAgent()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	result := Probe(context.Background(), config.Upstream{URL: server.URL}, testTimeouts())
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if result.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", result.StatusCode)
	}
	if result.Latency < 0 {
		t.Fatal("negative latency")
	}
	if !strings.HasPrefix(userAgent, "larder/") {
		t.Fatalf("user agent = %q", userAgent)
	}
}

func TestProbeConnectToAndHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	up := config.Upstream{
		URL:       "http://upstream.invalid:" + parsed.Port(),
		ConnectTo: parsed.Host,
		Headers:   map[string]string{"Authorization": "Bearer token"},
	}
	result := Probe(context.Background(), up, testTimeouts())
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", result.StatusCode)
	}
}

func TestProbeHTTPProxy(t *testing.T) {
	var seen atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(true)
		if r.URL.Host != "origin.invalid" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer proxy.Close()

	up := config.Upstream{URL: "http://origin.invalid/artifact", Proxy: config.Proxy(proxy.URL)}
	result := Probe(context.Background(), up, testTimeouts())
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if result.StatusCode != http.StatusAccepted || !seen.Load() {
		t.Fatalf("status = %d, proxy seen = %v", result.StatusCode, seen.Load())
	}
}

func TestProbeEnvProxy(t *testing.T) {
	var seen atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	up := config.Upstream{URL: "http://origin.invalid/", Proxy: config.ProxyEnv}
	result := Probe(context.Background(), up, testTimeouts())
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if result.StatusCode != http.StatusOK || !seen.Load() {
		t.Fatalf("status = %d, proxy seen = %v", result.StatusCode, seen.Load())
	}
}

func TestProbeSOCKS5(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()
	parsedOrigin, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newFakeSOCKS(t, parsedOrigin.Host)

	tests := []struct {
		name   string
		url    string
		scheme string
	}{
		{"socks5", "http://" + parsedOrigin.Host, "socks5"},
		{"socks5h", "http://upstream.invalid:" + parsedOrigin.Port(), "socks5h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy.calls.Store(0)
			up := config.Upstream{URL: tt.url, Proxy: config.Proxy(tt.scheme + "://" + proxy.addr)}
			result := Probe(context.Background(), up, testTimeouts())
			if result.Err != nil {
				t.Fatal(result.Err)
			}
			if result.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", result.StatusCode)
			}
			if proxy.calls.Load() == 0 {
				t.Fatal("SOCKS proxy was not used")
			}
		})
	}
}

func TestProbeUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	timeouts := testTimeouts()
	timeouts.Connect = config.Duration(200 * time.Millisecond)
	result := Probe(context.Background(), config.Upstream{URL: "http://" + addr}, timeouts)
	if result.Err == nil {
		t.Fatal("closed port probe succeeded")
	}
}

func TestRedactError(t *testing.T) {
	proxy := config.Proxy("socks5h://alice:secret@127.0.0.1:1080")
	err := redactError(errors.New("dial alice:secret failed for alice"), proxy)
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "alice") {
		t.Fatalf("credentials leaked: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Fatalf("error was not redacted: %v", err)
	}
}

type fakeSOCKS struct {
	addr   string
	target string
	calls  atomic.Int64
	ln     net.Listener
	wg     sync.WaitGroup
}

func newFakeSOCKS(t *testing.T, target string) *fakeSOCKS {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSOCKS{addr: ln.Addr().String(), target: target, ln: ln}
	go f.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		f.wg.Wait()
	})
	return f
}

func (f *fakeSOCKS) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.handle(conn)
		}()
	}
}

func (f *fakeSOCKS) handle(client net.Conn) {
	defer client.Close()
	var header [2]byte
	if _, err := io.ReadFull(client, header[:]); err != nil {
		return
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(client, methods); err != nil {
		return
	}
	if _, err := client.Write([]byte{0x05, 0x00}); err != nil {
		return
	}
	var request [4]byte
	if _, err := io.ReadFull(client, request[:]); err != nil {
		return
	}
	if request[0] != 0x05 || request[1] != 0x01 {
		return
	}
	switch request[3] {
	case 0x01:
		if _, err := io.ReadFull(client, make([]byte, 4)); err != nil {
			return
		}
	case 0x03:
		var length [1]byte
		if _, err := io.ReadFull(client, length[:]); err != nil {
			return
		}
		if _, err := io.ReadFull(client, make([]byte, int(length[0]))); err != nil {
			return
		}
	case 0x04:
		if _, err := io.ReadFull(client, make([]byte, 16)); err != nil {
			return
		}
	default:
		return
	}
	if _, err := io.ReadFull(client, make([]byte, 2)); err != nil {
		return
	}

	upstream, err := net.Dial("tcp", f.target)
	if err != nil {
		_, _ = client.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	f.calls.Add(1)
	if _, err := client.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(upstream, client)
		_ = upstream.Close()
		close(done)
	}()
	_, _ = io.Copy(client, upstream)
	<-done
}
