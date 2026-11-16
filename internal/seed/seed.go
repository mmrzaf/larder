// Package seed parses seed files and resolves curated package and toolchain
// lists through a running Larder server.
package seed

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// File is a parsed seed document.
type File struct {
	Version    int            `yaml:"version"`
	PinSet     string         `yaml:"pin_set"`
	Go         *GoSection     `yaml:"go"`
	NPM        *NPMSection    `yaml:"npm"`
	PyPI       *PyPISection   `yaml:"pypi"`
	Crates     *CratesSection `yaml:"crates"`
	Rustup     *RustupSection `yaml:"rustup"`
	Maven      *MavenSection  `yaml:"maven"`
	GradleDist *GradleSection `yaml:"gradle_dist"`
	Apt        *AptSection    `yaml:"apt"`
	Generic    []GenericEntry `yaml:"generic"`
}

// GoSection lists modules and toolchains.
type GoSection struct {
	Modules    []string `yaml:"modules"`
	Toolchains []string `yaml:"toolchains"`
	Platforms  []string `yaml:"platforms"`
}

// NPMSection lists package specifications.
type NPMSection struct {
	Packages []string `yaml:"packages"`
}

// PyPISection lists packages and platform tuples.
type PyPISection struct {
	Packages   []string `yaml:"packages"`
	Python     []string `yaml:"python"`
	Platforms  []string `yaml:"platforms"`
	OnlyBinary bool     `yaml:"only_binary"`
}

// CratesSection lists crate specifications.
type CratesSection struct {
	Packages []string `yaml:"packages"`
}

// RustupSection lists channels, targets and components.
type RustupSection struct {
	Channels   []string `yaml:"channels"`
	Targets    []string `yaml:"targets"`
	Components []string `yaml:"components"`
}

// MavenSection lists coordinates for one Maven repository.
type MavenSection struct {
	Repo        string   `yaml:"repo"`
	Coordinates []string `yaml:"coordinates"`
	Transitive  bool     `yaml:"transitive"`
}

// GradleSection lists Gradle distributions.
type GradleSection struct {
	Repo     string   `yaml:"repo"`
	Versions []string `yaml:"versions"`
}

// AptSection lists packages for one archive suite.
type AptSection struct {
	Repo     string   `yaml:"repo"`
	Suite    string   `yaml:"suite"`
	Arch     []string `yaml:"arch"`
	Packages []string `yaml:"packages"`
}

// GenericEntry lists paths for one generic repository.
type GenericEntry struct {
	Repo  string   `yaml:"repo"`
	Paths []string `yaml:"paths"`
}

// Load parses one seed file strictly.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var file File
	if err := decoder.Decode(&file); err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("%s: empty seed file", path)
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if file.Version != 1 {
		return nil, fmt.Errorf("%s: version must be 1", path)
	}
	return &file, nil
}
