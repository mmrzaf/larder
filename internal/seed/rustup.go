package seed

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type rustupManifest struct {
	Date string `toml:"date"`
	Pkg  map[string]struct {
		Target map[string]struct {
			URL   string `toml:"url"`
			XzURL string `toml:"xz_url"`
		} `toml:"target"`
	} `toml:"pkg"`
}

func (r *resolver) resolveRustup(ctx context.Context) {
	section := r.section("rustup")
	section.Resolver = "direct"
	if len(r.file.Rustup.Channels) == 0 {
		section.Message = appendMessage(section.Message, "no channels listed; nothing fetched")
		return
	}
	if len(r.file.Rustup.Targets) == 0 {
		section.Message = appendMessage(section.Message, "no targets listed; nothing fetched")
		return
	}
	for _, channel := range r.file.Rustup.Channels {
		path := "/dist/channel-rust-" + channel + ".toml"
		r.add(section, "rustup", "", path, true, 0, "seed")
		data, err := r.fetchDocument(ctx, "rustup", path)
		if err != nil {
			section.Failed = true
			section.Message = appendMessage(section.Message, channel+": "+err.Error())
			continue
		}
		var manifest rustupManifest
		if err := toml.Unmarshal(data, &manifest); err != nil {
			section.Failed = true
			section.Message = appendMessage(section.Message, channel+": "+err.Error())
			continue
		}
		for _, component := range r.file.Rustup.Components {
			pkg, ok := manifest.Pkg[component]
			if !ok {
				section.Message = appendMessage(section.Message, channel+": component "+component+" is not in the manifest")
				continue
			}
			for _, target := range r.file.Rustup.Targets {
				entry, ok := pkg.Target[target]
				if !ok {
					section.Message = appendMessage(section.Message, channel+": "+component+" has no target "+target)
					continue
				}
				rawURL := entry.XzURL
				if rawURL == "" {
					rawURL = entry.URL
				}
				path, ok := rustupArtifactPath(rawURL)
				if !ok {
					continue
				}
				r.add(section, "rustup", "", path, false, 0, "seed")
				section.Resolved = append(section.Resolved, component+" "+target+" "+path)
			}
		}
	}
}

func rustupArtifactPath(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	path := parsed.EscapedPath()
	index := strings.Index(path, "/dist/")
	if index < 0 {
		return "", false
	}
	return path[index:], true
}

func (r *resolver) fetchDocument(ctx context.Context, kind, path string) ([]byte, error) {
	repo, ok := r.repoFor(kind, "")
	if !ok {
		return nil, fmt.Errorf("no %s repository configured", kind)
	}
	target := r.repoURL(repo) + path
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	response, err := r.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d from %s", response.StatusCode, path)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	return data, nil
}
