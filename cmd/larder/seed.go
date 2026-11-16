package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/seed"
	"github.com/mmrzaf/larder/internal/warm"
)

func runSeed(args []string, configPath, adminURL string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("seed", configPath, adminURL, jsonDefault, stderr)
	server := flags.fs.String("server", "", "Larder public base URL")
	concurrency := flags.fs.Int("concurrency", 4, "parallel downloads")
	pinSet := flags.fs.String("pin-set", "", "override the seed file pin set")
	dryRun := flags.fs.Bool("dry-run", false, "resolve and print without fetching")
	resolver := flags.fs.String("resolver", "", "native or lockfile")
	maxBytes := flags.fs.String("max-bytes", "", "maximum plan size")
	keepWorkdir := flags.fs.Bool("keep-workdir", false, "keep temporary tool directories")
	captureFrom := &stringList{}
	flags.fs.Var(captureFrom, "capture-from", "CIDR observed by the server")
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: larder seed [flags] <seedfile.yaml>...")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	base := *server
	if base == "" {
		derived, err := publicBase(cfg, config.Repo{})
		if err != nil {
			fmt.Fprintf(stderr, "public URL: %v\n", err)
			return exitUsage
		}
		base = derived
	}
	base = strings.TrimSuffix(base, "/")
	client, _, err := adminClient(cfg, *flags.admin)
	if err != nil || client == nil {
		fmt.Fprintln(stderr, "seed requires a running server with the admin listener enabled")
		return 3
	}
	if _, err := client.Stats(context.Background()); err != nil {
		fmt.Fprintln(stderr, "seed requires a running server: "+err.Error())
		return serverUnreachable(err)
	}
	repos := warmRepos(cfg)
	mounts := warmMounts(repos)
	httpClient := &http.Client{Timeout: 30 * time.Minute}
	from := *captureFrom
	if len(from) == 0 {
		from = localCIDRs()
	}
	failed := false
	for _, path := range flags.fs.Args() {
		file, err := seed.Load(path)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		effectivePin := *pinSet
		if effectivePin == "" {
			effectivePin = file.PinSet
		}
		token := ""
		if effectivePin != "" && !*dryRun {
			token, err = client.CaptureStart(context.Background(), effectivePin, from)
			if err != nil {
				fmt.Fprintf(stderr, "start capture: %v\n", err)
				return exitFailure
			}
		}
		result, err := seed.Resolve(context.Background(), file, seed.Options{
			BaseURL: base, Repos: repos, Resolver: *resolver,
			Concurrency: *concurrency, DryRun: *dryRun, KeepWorkdir: *keepWorkdir,
			Client: httpClient,
		})
		if err != nil {
			if token != "" {
				_, _, _, _ = client.CaptureStop(context.Background(), token, false)
			}
			fmt.Fprintf(stderr, "seed %s: %v\n", path, err)
			failed = true
			continue
		}
		if !*flags.json {
			printSeedSections(stdout, result.Sections)
		}
		total := estimatePlan(context.Background(), httpClient, base, mounts, result.Items)
		if *maxBytes != "" {
			limit, err := config.ParseSize(*maxBytes)
			if err != nil {
				fmt.Fprintf(stderr, "max-bytes: %v\n", err)
				if token != "" {
					_, _, _, _ = client.CaptureStop(context.Background(), token, false)
				}
				return exitUsage
			}
			if total > limit.Bytes() {
				fmt.Fprintf(stderr, "seed plan needs %d bytes, above --max-bytes %d\n", total, limit.Bytes())
				if token != "" {
					_, _, _, _ = client.CaptureStop(context.Background(), token, false)
				}
				return exitUsage
			}
		}
		if *dryRun {
			if *flags.json {
				if code := writeJSONOutput(stdout, map[string]any{
					"path": path, "pin_set": effectivePin, "sections": result.Sections,
					"items": result.Items, "total_bytes": total,
				}, stderr); code != exitOK {
					return code
				}
			} else {
				printSeedPlan(stdout, result.Items, total)
			}
			for _, section := range result.Sections {
				if section.Failed {
					failed = true
				}
			}
			continue
		}
		summary := warm.Execute(context.Background(), warm.Plan{Items: result.Items}, warm.ExecuteOptions{
			BaseURL: base, Mounts: mounts, Concurrency: *concurrency,
			Retries: 2, Token: token, Client: httpClient,
		})
		if token != "" {
			_, _, pinned, err := client.CaptureStop(context.Background(), token, true)
			if err != nil {
				fmt.Fprintf(stderr, "stop capture: %v\n", err)
				summary.Failed++
			} else {
				summary.Pinned = pinned
			}
		}
		if *flags.json {
			if code := writeJSONOutput(stdout, map[string]any{
				"path": path, "sections": result.Sections, "summary": summary,
			}, stderr); code != exitOK {
				return code
			}
		} else {
			printWarmSummary(stdout, summary)
			fmt.Fprintf(stdout, "pinned: %d\n", summary.Pinned)
		}
		for _, section := range result.Sections {
			if section.Failed {
				failed = true
			}
		}
		if summary.Failed > 0 {
			failed = true
		}
	}
	if failed {
		return exitFailure
	}
	return exitOK
}

func printSeedSections(stdout io.Writer, sections []seed.Section) {
	for _, section := range sections {
		status := "ok"
		if section.Failed {
			status = "failed"
		}
		fmt.Fprintf(stdout, "section %s: %s (resolver %s)\n", section.Name, status, section.Resolver)
		if section.Tool != "" {
			fmt.Fprintf(stdout, "  tool: %s\n", section.Tool)
		}
		if section.Message != "" {
			fmt.Fprintf(stdout, "  %s\n", section.Message)
		}
		for _, resolved := range section.Resolved {
			fmt.Fprintf(stdout, "  resolved: %s\n", resolved)
		}
	}
}

func printSeedPlan(stdout io.Writer, items []warm.Item, total int64) {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPO\tPATH\tSIZE\tKIND")
	for _, item := range items {
		kind := "artifact"
		if item.Metadata {
			kind = "metadata"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", item.Repo, item.Path, item.Size, kind)
	}
	_ = tw.Flush()
	fmt.Fprintf(stdout, "items: %d, known bytes: %d\n", len(items), total)
}
