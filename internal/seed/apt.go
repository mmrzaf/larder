package seed

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"strings"

	"github.com/ulikunitz/xz"
)

type aptPackage struct {
	Name     string
	Version  string
	Filename string
	Depends  string
	Provides string
}

func (r *resolver) resolveApt(ctx context.Context) {
	section := r.section("apt")
	section.Resolver = "direct"
	repo, ok := r.repoFor("apt", r.file.Apt.Repo)
	if !ok {
		section.Failed = true
		section.Message = appendMessage(section.Message, "no apt repository configured")
		return
	}
	suite := r.file.Apt.Suite
	arches := r.file.Apt.Arch
	if suite == "" {
		section.Failed = true
		section.Message = appendMessage(section.Message, "suite is required")
		return
	}
	if len(arches) == 0 {
		arches = []string{"amd64"}
	}
	r.add(section, "apt", repo.Name, "/dists/"+suite+"/InRelease", true, 0, "seed")
	r.add(section, "apt", repo.Name, "/dists/"+suite+"/Release", true, 0, "seed")
	for _, arch := range arches {
		r.add(section, "apt", repo.Name, "/dists/"+suite+"/main/binary-"+arch+"/Packages.gz", true, 0, "seed")
	}
	if r.opts.DryRun {
		section.Message = appendMessage(section.Message, "dependency closure requires resolution")
		return
	}
	var packages []aptPackage
	for _, arch := range arches {
		path := "/dists/" + suite + "/main/binary-" + arch + "/Packages.gz"
		data, err := r.fetchDocument(ctx, "apt", path)
		if err != nil {
			section.Message = appendMessage(section.Message, arch+": "+err.Error())
			continue
		}
		plain, err := decompressPackages(path, data)
		if err != nil {
			section.Message = appendMessage(section.Message, arch+": "+err.Error())
			continue
		}
		packages = append(packages, parseAptPackages(plain)...)
	}
	if len(packages) == 0 {
		section.Failed = true
		section.Message = appendMessage(section.Message, "no package metadata was readable")
		return
	}
	byName := make(map[string]aptPackage)
	provides := make(map[string]aptPackage)
	for _, pkg := range packages {
		byName[pkg.Name] = pkg
		for _, provided := range strings.Split(pkg.Provides, ",") {
			provided = strings.TrimSpace(provided)
			if provided == "" {
				continue
			}
			name := dependencyName(provided)
			if name != "" {
				if _, exists := provides[name]; !exists {
					provides[name] = pkg
				}
			}
		}
	}
	seen := make(map[string]bool)
	var queue []string
	for _, requested := range r.file.Apt.Packages {
		queue = append(queue, requested)
	}
	selected := make([]aptPackage, 0, len(queue))
	for len(queue) > 0 {
		spec := strings.TrimSpace(queue[0])
		queue = queue[1:]
		name, version := splitAptSpec(spec)
		pkg, ok := byName[name]
		if !ok {
			pkg, ok = provides[name]
		}
		if !ok {
			section.Message = appendMessage(section.Message, "package "+spec+" was not found")
			continue
		}
		if version != "" && pkg.Version != version {
			section.Message = appendMessage(section.Message, "package "+spec+" version is "+pkg.Version)
			continue
		}
		key := pkg.Name + "=" + pkg.Version
		if seen[key] {
			continue
		}
		seen[key] = true
		selected = append(selected, pkg)
		section.Resolved = append(section.Resolved, key)
		for _, group := range strings.Split(pkg.Depends, ",") {
			group = strings.TrimSpace(group)
			if group == "" || strings.HasPrefix(group, "${") {
				continue
			}
			for _, alternative := range strings.Split(group, "|") {
				name := dependencyName(alternative)
				if name == "" {
					continue
				}
				if _, exists := byName[name]; exists {
					queue = append(queue, name)
					break
				}
				if _, exists := provides[name]; exists {
					queue = append(queue, name)
					break
				}
			}
		}
	}
	for _, pkg := range selected {
		if pkg.Filename == "" {
			continue
		}
		r.add(section, "apt", repo.Name, "/"+strings.TrimPrefix(pkg.Filename, "/"), false, 0, "seed")
	}
	if len(selected) == 0 {
		section.Failed = true
	}
}

func splitAptSpec(spec string) (string, string) {
	name, version, ok := strings.Cut(spec, "=")
	if !ok {
		return spec, ""
	}
	return name, version
}

func dependencyName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "!") {
		return ""
	}
	if index := strings.IndexByte(value, '('); index >= 0 {
		value = value[:index]
	}
	value = strings.TrimSpace(value)
	if index := strings.IndexByte(value, ':'); index >= 0 {
		value = value[:index]
	}
	return strings.TrimSpace(value)
}

func parseAptPackages(data []byte) []aptPackage {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	var out []aptPackage
	var current aptPackage
	flush := func() {
		if current.Name != "" {
			out = append(out, current)
		}
		current = aptPackage{}
	}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "Package":
			current.Name = value
		case "Version":
			current.Version = value
		case "Filename":
			current.Filename = value
		case "Depends":
			current.Depends = value
		case "Provides":
			current.Provides = value
		}
	}
	flush()
	return out
}

func decompressPackages(path string, data []byte) ([]byte, error) {
	switch {
	case strings.HasSuffix(path, ".gz"):
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer func() { _ = reader.Close() }()
		return io.ReadAll(io.LimitReader(reader, 256<<20))
	case strings.HasSuffix(path, ".xz"):
		reader, err := xz.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return io.ReadAll(io.LimitReader(reader, 256<<20))
	default:
		return data, nil
	}
}
