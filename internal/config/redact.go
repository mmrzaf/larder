package config

import (
	"net/url"
	"strings"
)

// Redact returns a copy with header values and proxy credentials masked.
func (c *Config) Redact() *Config {
	if c == nil {
		return nil
	}
	out := *c
	out.PublicBaseURL = RedactURL(out.PublicBaseURL)
	out.UpstreamProxy = redactProxy(out.UpstreamProxy)
	out.Access.AllowCIDRs = cloneStrings(c.Access.AllowCIDRs)
	out.Repos = make([]Repo, len(c.Repos))
	for i := range c.Repos {
		out.Repos[i] = redactRepo(c.Repos[i])
	}
	return &out
}

func redactRepo(in Repo) Repo {
	out := in
	out.PublicURL = RedactURL(out.PublicURL)
	out.Proxy = redactProxy(out.Proxy)
	out.Protect = cloneStrings(in.Protect)
	out.Upstreams = make([]Upstream, len(in.Upstreams))
	for i := range in.Upstreams {
		out.Upstreams[i] = redactUpstream(in.Upstreams[i])
	}
	if in.Go != nil {
		g := *in.Go
		g.SumDBUpstreams = cloneStringMap(in.Go.SumDBUpstreams)
		out.Go = &g
	}
	if in.Maven != nil {
		m := *in.Maven
		out.Maven = &m
	}
	if in.NPM != nil {
		n := *in.NPM
		out.NPM = &n
	}
	if in.PyPI != nil {
		p := *in.PyPI
		p.FilesHosts = cloneStrings(in.PyPI.FilesHosts)
		out.PyPI = &p
	}
	if in.Crates != nil {
		cr := *in.Crates
		cr.DLUpstreams = cloneStrings(in.Crates.DLUpstreams)
		out.Crates = &cr
	}
	if in.APT != nil {
		a := *in.APT
		out.APT = &a
	}
	if in.Generic != nil {
		g := *in.Generic
		g.Rules = append([]GenericRule(nil), in.Generic.Rules...)
		g.Expect = append([]GenericExpect(nil), in.Generic.Expect...)
		g.Companion = append([]GenericCompanion(nil), in.Generic.Companion...)
		out.Generic = &g
	}
	return out
}

func redactUpstream(in Upstream) Upstream {
	out := in
	out.URL = RedactURL(out.URL)
	out.Proxy = redactProxy(out.Proxy)
	if in.Headers != nil {
		out.Headers = make(map[string]string, len(in.Headers))
		for k := range in.Headers {
			out.Headers[k] = "***"
		}
	}
	if in.Timeouts != nil {
		t := *in.Timeouts
		out.Timeouts = &t
	}
	return out
}

func redactProxy(p Proxy) Proxy {
	if p == ProxyDirect || p == ProxyEnv || p == "" {
		return p
	}
	u, err := url.Parse(string(p))
	if err != nil {
		return "***"
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	return Proxy(redactedURL(u))
}

// RedactURL masks credentials embedded in a URL.
func RedactURL(value string) string {
	if value == "" || !strings.Contains(value, "@") {
		return value
	}
	u, err := url.Parse(value)
	if err != nil || u.User == nil {
		return value
	}
	u.User = url.User("***")
	return redactedURL(u)
}

func redactedURL(u *url.URL) string {
	return strings.ReplaceAll(u.String(), "%2A%2A%2A", "***")
}
