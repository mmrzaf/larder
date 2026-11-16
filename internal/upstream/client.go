package upstream

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/http/httpproxy"

	"github.com/mmrzaf/larder/internal/config"
)

// Options configures the HTTP client built for one upstream.
type Options struct {
	Timeouts              config.Timeouts
	MaxConnsPerHost       int
	BlockPrivateRedirects bool
	AllowedHosts          []string
}

// Upstream is a configured HTTP origin and its reusable client.
type Upstream struct {
	Base      *url.URL
	Proxy     config.Proxy
	Headers   http.Header
	ConnectTo string
	CAFile    string
	Timeouts  config.Timeouts

	client *http.Client
	key    string
	name   string
}

type restrictHostsKey struct{}

// Target is one URL to try with the connection settings of an upstream.
type Target struct {
	Repo          string
	Name          string
	Key           string
	URL           *url.URL
	Headers       http.Header
	Body          []byte
	RestrictHosts bool

	upstream *Upstream
}

// New builds an upstream client.
func New(cfg config.Upstream, opts Options) (*Upstream, error) {
	base, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse upstream URL: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" || base.Host == "" {
		return nil, fmt.Errorf("upstream URL must be absolute http or https")
	}
	if base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("upstream URL must not contain a query or fragment")
	}
	allowed := make(map[string]bool, len(opts.AllowedHosts)+1)
	for _, host := range opts.AllowedHosts {
		allowed[hostOnly(host)] = true
	}
	allowed[hostOnly(base.Host)] = true

	u := &Upstream{
		Base:      base,
		Proxy:     cfg.Proxy,
		Headers:   headerMap(cfg.Headers),
		ConnectTo: cfg.ConnectTo,
		CAFile:    cfg.CAFile,
		Timeouts:  cfg.Timeouts.Apply(opts.Timeouts),
		key:       base.String(),
		name:      base.Host,
	}
	u.client, err = newHTTPClient(u, opts, allowed)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// Name returns the upstream host used in response headers and metrics.
func (u *Upstream) Name() string { return u.name }

// Key returns the circuit breaker key.
func (u *Upstream) Key() string { return u.key }

// Target builds a target under this upstream's base URL.
func (u *Upstream) Target(repo, path string, headers http.Header) (Target, error) {
	full, err := joinURL(u.Base, path)
	if err != nil {
		return Target{}, err
	}
	return Target{
		Repo:     repo,
		Name:     u.name,
		Key:      u.key,
		URL:      full,
		Headers:  cloneHeader(headers),
		upstream: u,
	}, nil
}

// WithPath returns a target on the same upstream with a different path.
func (t Target) WithPath(path string, headers http.Header) (Target, error) {
	if t.upstream == nil {
		return Target{}, fmt.Errorf("target has no upstream")
	}
	return t.upstream.Target(t.Repo, path, headers)
}

// ResolvedTarget builds a target from a complete URL using this upstream's connection settings.
func (u *Upstream) ResolvedTarget(repo, rawURL string, headers http.Header) (Target, error) {
	full, err := url.Parse(rawURL)
	if err != nil {
		return Target{}, fmt.Errorf("parse resolved URL: %w", err)
	}
	if full.Scheme != "http" && full.Scheme != "https" || full.Host == "" {
		return Target{}, fmt.Errorf("resolved URL must be absolute http or https")
	}
	return Target{
		Repo:     repo,
		Name:     full.Host,
		Key:      hostOnly(full.Host),
		URL:      full,
		Headers:  cloneHeader(headers),
		upstream: u,
	}, nil
}

// CloseIdleConnections closes pooled connections for this upstream.
func (u *Upstream) CloseIdleConnections() {
	if u != nil && u.client != nil {
		u.client.CloseIdleConnections()
	}
}

func newHTTPClient(u *Upstream, opts Options, allowed map[string]bool) (*http.Client, error) {
	dialer := &net.Dialer{Timeout: u.Timeouts.Connect.Duration()}
	directDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if u.ConnectTo != "" {
			addr = u.ConnectTo
		}
		return dialer.DialContext(ctx, network, addr)
	}

	proxyValue := u.Proxy
	if proxyValue == "" {
		proxyValue = config.ProxyDirect
	}
	transport := &http.Transport{
		DialContext:           directDial,
		TLSHandshakeTimeout:   u.Timeouts.TLSHandshake.Duration(),
		ResponseHeaderTimeout: u.Timeouts.ResponseHeader.Duration(),
		IdleConnTimeout:       u.Timeouts.IdleTimeout.Duration(),
		MaxIdleConnsPerHost:   opts.MaxConnsPerHost,
		MaxConnsPerHost:       opts.MaxConnsPerHost,
		ForceAttemptHTTP2:     true,
	}
	if u.CAFile != "" {
		pool, err := loadCertPool(u.CAFile)
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
		proxyURL, err := url.Parse(string(proxyValue))
		if err != nil {
			return nil, fmt.Errorf("parse proxy: %w", err)
		}
		switch proxyURL.Scheme {
		case "http", "https":
			transport.Proxy = http.ProxyURL(proxyURL)
			transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, addr)
			}
		case "socks5", "socks5h":
			transport.DialContext, err = socksDialer(proxyURL, u.ConnectTo, u.Timeouts.Connect.Duration(), proxyURL.Scheme == "socks5h")
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported proxy scheme %q", proxyURL.Scheme)
		}
	}

	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return checkRedirect(req, via, allowed, opts.BlockPrivateRedirects)
		},
	}
	if u.Timeouts.FetchTotal > 0 {
		client.Timeout = u.Timeouts.FetchTotal.Duration()
	}
	return client, nil
}

func checkRedirect(req *http.Request, via []*http.Request, allowed map[string]bool, blockPrivate bool) error {
	if len(via) >= 5 {
		return errors.New("stopped after 5 redirects")
	}
	if len(via) > 0 {
		previous := via[len(via)-1]
		if !strings.EqualFold(previous.URL.Host, req.URL.Host) {
			req.Header.Del("Authorization")
		}
	}
	if allowed[hostOnly(req.URL.Host)] {
		return nil
	}
	if _, restricted := req.Context().Value(restrictHostsKey{}).(bool); restricted {
		return fmt.Errorf("redirect to host %s is outside the allowed set", req.URL.Host)
	}
	if !blockPrivate {
		return nil
	}
	private, err := privateHost(req.Context(), req.URL.Host)
	if err != nil {
		return fmt.Errorf("resolve redirect host: %w", err)
	}
	if private {
		return fmt.Errorf("redirect to private address %s refused", req.URL.Host)
	}
	return nil
}

func privateHost(ctx context.Context, hostport string) (bool, error) {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return isPrivateIP(ip), nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return false, err
	}
	for _, ip := range ips {
		if isPrivateIP(ip.IP) {
			return true, nil
		}
	}
	return false, nil
}

func isPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

func joinURL(base *url.URL, path string) (*url.URL, error) {
	baseText := strings.TrimSuffix(base.String(), "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	full, err := url.Parse(baseText + path)
	if err != nil {
		return nil, fmt.Errorf("build upstream URL: %w", err)
	}
	return full, nil
}

func headerMap(values map[string]string) http.Header {
	if len(values) == 0 {
		return nil
	}
	out := make(http.Header, len(values))
	for key, value := range values {
		out.Set(key, value)
	}
	return out
}

func cloneHeader(in http.Header) http.Header {
	if in == nil {
		return nil
	}
	return in.Clone()
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.ToLower(h)
	}
	return strings.ToLower(strings.Trim(hostport, "[]"))
}
