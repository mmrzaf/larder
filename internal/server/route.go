package server

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/mmrzaf/larder/internal/config"
)

func route(cfg *config.Config, r *http.Request, normalized string) (*config.Repo, string, error) {
	if cfg.Routing == config.RoutingHost || cfg.Routing == config.RoutingBoth {
		host := requestHost(cfg, r)
		for i := range cfg.Repos {
			repo := &cfg.Repos[i]
			if repo.Host != "" && strings.EqualFold(hostOnly(repo.Host), hostOnly(host)) {
				return repo, normalized, nil
			}
		}
		if cfg.Routing == config.RoutingHost {
			return nil, "", fmt.Errorf("no host route")
		}
	}

	var best *config.Repo
	bestLen := -1
	for i := range cfg.Repos {
		repo := &cfg.Repos[i]
		if repo.Mount == "" {
			continue
		}
		if _, ok := StripMount(normalized, repo.Mount); !ok {
			continue
		}
		if len(repo.Mount) > bestLen {
			best = repo
			bestLen = len(repo.Mount)
		}
	}
	if best == nil {
		return nil, "", fmt.Errorf("no mount route")
	}
	pathValue, _ := StripMount(normalized, best.Mount)
	if pathValue == "" {
		pathValue = "/"
	}
	return best, pathValue, nil
}

func requestHost(cfg *config.Config, r *http.Request) string {
	if cfg.TrustForwarded {
		if host := firstHeader(r.Header.Get("X-Forwarded-Host")); host != "" {
			return host
		}
	}
	return r.Host
}

func computePublicBase(cfg *config.Config, repo *config.Repo, r *http.Request) string {
	if repo.PublicURL != "" {
		return strings.TrimSuffix(repo.PublicURL, "/")
	}
	if cfg.PublicBaseURL != "" {
		return strings.TrimSuffix(cfg.PublicBaseURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if cfg.TrustForwarded {
		if proto := firstHeader(r.Header.Get("X-Forwarded-Proto")); proto != "" {
			scheme = proto
		}
		if forwarded := firstHeader(r.Header.Get("X-Forwarded-Host")); forwarded != "" {
			host = forwarded
		}
	}
	return scheme + "://" + host
}

func checkFileHost(cfg *config.Config, repo *config.Repo, pathValue string) error {
	if repo.Kind != "pypi" && repo.PyPI == nil {
		return nil
	}
	rest, ok := StripMount(pathValue, "/_files")
	if !ok || rest == "/" {
		return nil
	}
	host := strings.SplitN(strings.TrimPrefix(rest, "/"), "/", 2)[0]
	if decoded, err := url.PathUnescape(host); err == nil {
		host = decoded
	}
	if host == "" {
		return fmt.Errorf("missing file host")
	}
	allowed := make(map[string]bool)
	for _, upstream := range repo.Upstreams {
		if parsed, err := parseURLHost(upstream.URL); err == nil {
			allowed[parsed] = true
		}
	}
	if repo.PyPI != nil {
		for _, item := range repo.PyPI.FilesHosts {
			allowed[hostOnly(item)] = true
		}
	}
	if !allowed[hostOnly(host)] {
		return fmt.Errorf("file host %s is not allowed", host)
	}
	return nil
}

func parseURLHost(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	return hostOnly(parsed.Host), nil
}

func hostOnly(value string) string {
	if host, _, err := net.SplitHostPort(value); err == nil {
		return strings.ToLower(strings.Trim(host, "[]"))
	}
	return strings.ToLower(strings.Trim(value, "[]"))
}

func firstHeader(value string) string {
	if value == "" {
		return ""
	}
	return strings.TrimSpace(strings.Split(value, ",")[0])
}
