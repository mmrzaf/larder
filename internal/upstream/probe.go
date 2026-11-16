// Package upstream implements outbound HTTP access to configured upstreams.
package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/net/http/httpproxy"
	xproxy "golang.org/x/net/proxy"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/version"
)

// ProbeResult is the outcome of one reachability check.
type ProbeResult struct {
	URL        string
	StatusCode int
	Latency    time.Duration
	Err        error
}

// Probe sends a HEAD request to an upstream base URL and reports reachability.
func Probe(ctx context.Context, up config.Upstream, base config.Timeouts) ProbeResult {
	start := time.Now()
	timeouts := up.Timeouts.Apply(base)
	result := ProbeResult{URL: config.RedactURL(up.URL)}

	parsed, err := url.Parse(up.URL)
	if err != nil {
		result.Err = fmt.Errorf("parse upstream URL: %w", err)
		result.Latency = time.Since(start)
		return result
	}
	transport, err := buildTransport(up, timeouts)
	if err != nil {
		result.Err = err
		result.Latency = time.Since(start)
		return result
	}
	defer transport.CloseIdleConnections()

	timeout := timeouts.Connect.Duration() + timeouts.TLSHandshake.Duration() + timeouts.ResponseHeader.Duration()
	if timeouts.FetchTotal > 0 && timeouts.FetchTotal.Duration() < timeout {
		timeout = timeouts.FetchTotal.Duration()
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, parsed.String(), nil)
	if err != nil {
		result.Err = fmt.Errorf("create probe request: %w", err)
		result.Latency = time.Since(start)
		return result
	}
	for name, value := range up.Headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("User-Agent", userAgent())

	resp, err := client.Do(req)
	result.Latency = time.Since(start)
	if err != nil {
		result.Err = redactError(err, up.Proxy)
		return result
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	result.StatusCode = resp.StatusCode
	return result
}

func buildTransport(up config.Upstream, timeouts config.Timeouts) (*http.Transport, error) {
	dialer := &net.Dialer{Timeout: timeouts.Connect.Duration()}
	directDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if up.ConnectTo != "" {
			addr = up.ConnectTo
		}
		return dialer.DialContext(ctx, network, addr)
	}

	proxyValue := up.Proxy
	if proxyValue == "" {
		proxyValue = config.ProxyDirect
	}

	transport := &http.Transport{
		DialContext:           directDial,
		TLSHandshakeTimeout:   timeouts.TLSHandshake.Duration(),
		ResponseHeaderTimeout: timeouts.ResponseHeader.Duration(),
		IdleConnTimeout:       timeouts.IdleTimeout.Duration(),
		DisableCompression:    true,
	}
	if up.CAFile != "" {
		pool, err := loadCertPool(up.CAFile)
		if err != nil {
			return nil, err
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}

	switch proxyValue {
	case config.ProxyDirect:
	case config.ProxyEnv:
		proxyFunc := httpproxy.FromEnvironment().ProxyFunc()
		transport.Proxy = func(req *http.Request) (*url.URL, error) { return proxyFunc(req.URL) }
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
	default:
		u, err := url.Parse(string(proxyValue))
		if err != nil {
			return nil, fmt.Errorf("parse proxy: %w", err)
		}
		switch u.Scheme {
		case "http", "https":
			transport.Proxy = http.ProxyURL(u)
			transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, addr)
			}
		case "socks5", "socks5h":
			transport.DialContext, err = socksDialer(u, up.ConnectTo, timeouts.Connect.Duration(), u.Scheme == "socks5h")
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
		}
	}
	return transport, nil
}

func socksDialer(proxyURL *url.URL, connectTo string, timeout time.Duration, remoteDNS bool) (func(context.Context, string, string) (net.Conn, error), error) {
	address := proxyURL.Host
	if _, _, err := net.SplitHostPort(address); err != nil {
		host := address
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
		}
		address = net.JoinHostPort(host, "1080")
	}
	var auth *xproxy.Auth
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		auth = &xproxy.Auth{User: proxyURL.User.Username(), Password: password}
	}
	dialer, err := xproxy.SOCKS5("tcp", address, auth, &net.Dialer{Timeout: timeout})
	if err != nil {
		return nil, fmt.Errorf("create SOCKS5 dialer: %w", err)
	}

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if connectTo != "" {
			addr = connectTo
		}
		if !remoteDNS {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("split SOCKS target: %w", err)
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("resolve SOCKS target %s: %w", host, err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("resolve SOCKS target %s: no addresses", host)
			}
			addr = net.JoinHostPort(ips[0].IP.String(), port)
		}
		if contextDialer, ok := dialer.(xproxy.ContextDialer); ok {
			return contextDialer.DialContext(ctx, network, addr)
		}
		return dialer.Dial(network, addr)
	}, nil
}

func loadCertPool(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ca_file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("ca_file %s contains no PEM certificates", path)
	}
	return pool, nil
}

func userAgent() string {
	return fmt.Sprintf("larder/%s (+https://github.com/mmrzaf/larder)", version.Version)
}

func redactError(err error, proxy config.Proxy) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	if proxy != "" && proxy != config.ProxyDirect && proxy != config.ProxyEnv {
		if u, parseErr := url.Parse(string(proxy)); parseErr == nil && u.User != nil {
			password, hasPassword := u.User.Password()
			if hasPassword && password != "" {
				message = strings.ReplaceAll(message, password, "***")
			}
			message = strings.ReplaceAll(message, u.User.String(), "***")
			message = strings.ReplaceAll(message, u.User.Username(), "***")
		}
	}
	return errors.New(message)
}
