package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mmrzaf/larder/internal/config"
)

func TestProbeInvalidInputs(t *testing.T) {
	timeouts := testTimeouts()
	if result := Probe(context.Background(), config.Upstream{URL: "http://[::1"}, timeouts); result.Err == nil {
		t.Fatal("invalid URL accepted")
	}
	if result := Probe(context.Background(), config.Upstream{URL: "http://example.invalid", Proxy: "ftp://proxy"}, timeouts); result.Err == nil {
		t.Fatal("unsupported proxy scheme accepted")
	}
	if result := Probe(context.Background(), config.Upstream{URL: "http://example.invalid", Proxy: "http://proxy"}, timeouts); result.Err == nil {
		t.Fatal("unreachable proxy probe succeeded")
	}
}

func TestSocksDialerAddressDefaultsAndErrors(t *testing.T) {
	dial, err := socksDialer(&url.URL{Scheme: "socks5", Host: "127.0.0.1"}, "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dial(context.Background(), "tcp", "bad host:80"); err == nil {
		t.Fatal("invalid local DNS name resolved")
	}

	dial, err = socksDialer(&url.URL{Scheme: "socks5", Host: "127.0.0.1"}, "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dial(context.Background(), "tcp", "127.0.0.1:notaport"); err == nil {
		t.Fatal("invalid port accepted")
	}

	dial, err = socksDialer(&url.URL{Scheme: "socks5", Host: "127.0.0.1"}, "no-port", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dial(context.Background(), "tcp", "example.com:80"); err == nil {
		t.Fatal("invalid connect_to accepted")
	}
}

func TestProbeRedirectLimit(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL, http.StatusFound)
	}))
	defer server.Close()

	result := Probe(context.Background(), config.Upstream{URL: server.URL}, testTimeouts())
	if result.Err == nil || !strings.Contains(result.Err.Error(), "redirect") {
		t.Fatalf("redirect probe error = %v", result.Err)
	}
}

func TestProbeHeaderTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)

	timeouts := testTimeouts()
	timeouts.Connect = config.Duration(100 * time.Millisecond)
	timeouts.TLSHandshake = config.Duration(100 * time.Millisecond)
	timeouts.ResponseHeader = config.Duration(50 * time.Millisecond)
	result := Probe(context.Background(), config.Upstream{URL: server.URL}, timeouts)
	if result.Err == nil {
		t.Fatal("slow headers probe succeeded")
	}
}

func TestRedactErrorWithoutCredentials(t *testing.T) {
	err := redactError(nil, config.ProxyDirect)
	if err != nil {
		t.Fatal(err)
	}
	original := context.DeadlineExceeded
	if got := redactError(original, config.ProxyDirect); got.Error() != original.Error() {
		t.Fatalf("error changed: %v", got)
	}
}
