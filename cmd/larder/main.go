// Command larder is the single binary for the Larder package cache.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/upstream"
	"github.com/mmrzaf/larder/internal/version"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	configPath := os.Getenv("LARDER_CONFIG")
	if configPath == "" {
		configPath = "./larder.yaml"
	}

	global := flag.NewFlagSet("larder", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.StringVar(&configPath, "c", configPath, "configuration file")
	global.StringVar(&configPath, "config", configPath, "configuration file")
	adminURL := global.String("admin", "", "admin API URL")
	var jsonOutput bool
	global.BoolVar(&jsonOutput, "json", false, "machine-readable output")
	verbose := global.Bool("v", false, "debug logging")
	global.Usage = func() { printUsage(stderr) }

	if err := global.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitUsage
	}
	if *verbose {
		_ = os.Setenv("LARDER_LOG_LEVEL", "debug")
	}
	rest := global.Args()
	if len(rest) == 0 {
		printUsage(stderr)
		return exitUsage
	}

	switch rest[0] {
	case "version":
		return runVersion(rest[1:], jsonOutput, stdout, stderr)
	case "serve":
		return runServe(rest[1:], configPath, stdout, stderr)
	case "healthcheck":
		return runHealthcheck(rest[1:], configPath, stdout, stderr)
	case "config":
		return runConfig(rest[1:], configPath, stdout, stderr)
	case "stats", "du", "ls", "objects", "rollback", "pin", "unpin", "pins",
		"gc", "evict", "prune", "offline", "reload", "fetch",
		"verify", "reindex", "snapshot", "warm", "seed", "export", "import", "ingest":
		return runStorage(rest, configPath, *adminURL, jsonOutput, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", rest[0])
		printUsage(stderr)
		return exitUsage
	}
}

func runVersion(args []string, jsonDefault bool, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", jsonDefault, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "version does not accept arguments")
		return exitUsage
	}

	info := version.Get()
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(info); err != nil {
			fmt.Fprintf(stderr, "write version: %v\n", err)
			return exitFailure
		}
		return exitOK
	}
	if _, err := fmt.Fprintf(stdout, "larder %s (commit %s, built %s, %s)\n",
		info.Version, info.Commit, info.Date, info.GoVersion); err != nil {
		fmt.Fprintf(stderr, "write version: %v\n", err)
		return exitFailure
	}
	return exitOK
}

func runConfig(args []string, configPath string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "config requires a subcommand: check, print or defaults")
		return exitUsage
	}
	switch args[0] {
	case "check":
		return runConfigCheck(args[1:], configPath, stdout, stderr)
	case "print":
		return runConfigPrint(args[1:], configPath, stdout, stderr)
	case "defaults":
		return runConfigDefaults(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown config subcommand %q\n", args[0])
		return exitUsage
	}
}

func runConfigCheck(args []string, configPath string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	connect := fs.Bool("connect", false, "check upstream reachability")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "config check does not accept arguments")
		return exitUsage
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	if !*connect {
		return exitOK
	}

	unreachable := false
	for i := range cfg.Repos {
		repo := &cfg.Repos[i]
		for j := range repo.Upstreams {
			result := upstream.Probe(context.Background(), repo.Upstreams[j], cfg.Timeouts)
			if result.Err != nil {
				unreachable = true
				if _, err := fmt.Fprintf(stdout, "%s %s: unreachable: %v\n", repo.Name, result.URL, result.Err); err != nil {
					fmt.Fprintf(stderr, "write check result: %v\n", err)
					return exitFailure
				}
				continue
			}
			if _, err := fmt.Fprintf(stdout, "%s %s: reachable (status %d) in %dms\n",
				repo.Name, result.URL, result.StatusCode, result.Latency.Milliseconds()); err != nil {
				fmt.Fprintf(stderr, "write check result: %v\n", err)
				return exitFailure
			}
		}
	}
	if unreachable {
		return exitFailure
	}
	return exitOK
}

func runConfigPrint(args []string, configPath string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config print", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "config print does not accept arguments")
		return exitUsage
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	enc := yaml.NewEncoder(stdout)
	enc.SetIndent(2)
	if err := enc.Encode(cfg.Redact()); err != nil {
		fmt.Fprintf(stderr, "write configuration: %v\n", err)
		return exitFailure
	}
	if err := enc.Close(); err != nil {
		fmt.Fprintf(stderr, "write configuration: %v\n", err)
		return exitFailure
	}
	return exitOK
}

func runConfigDefaults(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config defaults", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "config defaults does not accept arguments")
		return exitUsage
	}
	data, err := config.DefaultYAML()
	if err != nil {
		fmt.Fprintf(stderr, "write defaults: %v\n", err)
		return exitFailure
	}
	if _, err := stdout.Write(data); err != nil {
		fmt.Fprintf(stderr, "write defaults: %v\n", err)
		return exitFailure
	}
	return exitOK
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `usage: larder [-c FILE] [--json] <command> [arguments]

commands:
  serve              run the public and administrative listeners
  healthcheck        check the admin readiness endpoint
  version            print build information
  config check       validate a configuration file
  config print       print the effective configuration with secrets redacted
  config defaults    print an annotated configuration with every default
  stats              storage budget, usage and object totals
  du                 usage grouped by repo, class, age or size
  ls                 list cached objects
  objects            show one object and its history
  rollback           restore an earlier metadata generation
  pin, unpin, pins   manage pin sets
  gc, evict, prune   reclaim storage
  offline            read or change offline mode on a running server
  reload             reload configuration on a running server
  fetch              request paths through a running server
  verify             check stored blobs against their digests
  reindex            check or repair the index against the blob store
  snapshot           snapshot or restore the index
  warm               prefetch project lockfile artifacts
  seed               fetch a curated seed file
  export, import     move cached content between installations
  ingest             store files from a local cache tree`)
}
