package testutil

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	xproxy "golang.org/x/net/proxy"
)

func TestFakeUpstreamRoutes(t *testing.T) {
	f := NewFakeUpstream(t)
	f.Set("/ok", Route{Body: []byte("ok")})
	resp, err := http.Get(f.URL() + "/ok")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(body) != "ok" || f.Count("/ok") != 1 {
		t.Fatalf("ok route = %q, %v, count %d", body, err, f.Count("/ok"))
	}

	f.Set("/etag", Route{Body: []byte("etag"), ETag: `"v1"`, Conditional: true})
	req, _ := http.NewRequest(http.MethodGet, f.URL()+"/etag", nil)
	req.Header.Set("If-None-Match", `"v1"`)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional status = %d", resp.StatusCode)
	}

	f.Set("/flap", Route{Body: []byte("flap"), Flap: 1, FailStatus: http.StatusBadGateway})
	for i, want := range []int{http.StatusBadGateway, http.StatusOK} {
		resp, err = http.Get(f.URL() + "/flap")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("flap %d status = %d, want %d", i, resp.StatusCode, want)
		}
	}

	f.Set("/missing-then-ok", Route{Body: []byte("found"), NotFoundFirst: 1})
	resp, _ = http.Get(f.URL() + "/missing-then-ok")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("not-found-first status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	resp, _ = http.Get(f.URL() + "/missing-then-ok")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("not-found-first second status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	f.Set("/html", Route{HTML: true})
	resp, _ = http.Get(f.URL() + "/html")
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("html content type = %q", resp.Header.Get("Content-Type"))
	}
	_ = resp.Body.Close()
}

func TestFakeUpstreamFaults(t *testing.T) {
	f := NewFakeUpstream(t)
	f.Set("/truncate", Route{Body: []byte("short"), Truncate: true})
	resp, err := http.Get(f.URL() + "/truncate")
	if err == nil {
		_, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr == nil {
			t.Fatal("truncated response read without error")
		}
	}

	f.Set("/close", Route{Close: true})
	if resp, err := http.Get(f.URL() + "/close"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("closed connection returned a response")
	}

	f.Set("/stall", Route{Body: []byte("stalled"), Stall: true, StallAfter: 2})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.URL()+"/stall", nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func TestHTTPProxyForwards(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "proxied")
	}))
	defer origin.Close()
	proxy := NewHTTPProxy(t)
	proxyURL, _ := url.Parse(proxy.URL())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	resp, err := client.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(body) != "proxied" || proxy.Count() == 0 {
		t.Fatalf("proxy body = %q, err %v, count %d", body, err, proxy.Count())
	}
}

func TestSOCKS5ProxyForwards(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "socks")
	}))
	defer origin.Close()
	proxy := NewSOCKS5Proxy(t)
	dialer, err := xproxy.SOCKS5("tcp", proxy.Addr, nil, xproxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.(xproxy.ContextDialer).DialContext(ctx, network, addr)
		},
	}}
	resp, err := client.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || !bytes.Equal(body, []byte("socks")) || proxy.Count() == 0 {
		t.Fatalf("socks body = %q, err %v, count %d", body, err, proxy.Count())
	}
}
