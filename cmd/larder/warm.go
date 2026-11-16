package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/warm"
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func runWarm(args []string, configPath, adminURL string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("warm", configPath, adminURL, jsonDefault, stderr)
	server := flags.fs.String("server", "", "Larder public base URL")
	concurrency := flags.fs.Int("concurrency", 4, "parallel downloads")
	pinSet := flags.fs.String("pin-set", "", "pin fetched keys into this set")
	maxBytes := flags.fs.String("max-bytes", "", "maximum plan size")
	dryRun := flags.fs.Bool("dry-run", false, "print the plan and exit")
	only := flags.fs.String("only", "", "restrict ecosystems")
	platform := flags.fs.String("platform", "", "optional platform filter")
	repoMap := flags.fs.String("repo-map", "", "kind=repo overrides")
	fromCache := &stringList{}
	flags.fs.Var(fromCache, "from-cache", "derive requests from a client cache KIND[=PATH]")
	urlList := flags.fs.String("url-list", "", "file with generic repository paths")
	repoName := flags.fs.String("repo", "", "generic repository for --url-list")
	failFast := flags.fs.Bool("fail-fast", false, "stop at the first failure")
	retries := flags.fs.Int("retries", 2, "retries per item")
	force := flags.fs.Bool("force", false, "ignore the high watermark budget check")
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() == 0 && *urlList == "" && len(*fromCache) == 0 {
		fmt.Fprintln(stderr, "usage: larder warm [flags] <path>...")
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
		fmt.Fprintln(stderr, "warm requires a running server with the admin listener enabled")
		return 3
	}
	if _, err := client.Stats(context.Background()); err != nil {
		fmt.Fprintln(stderr, "warm requires a running server: "+err.Error())
		return serverUnreachable(err)
	}
	repos := warmRepos(cfg)
	opts := warm.Options{
		Paths: flags.fs.Args(), Repos: repos, RepoMap: parseRepoMap(*repoMap),
		Only: splitCSV(*only), FromCache: *fromCache, URLList: *urlList, URLRepo: *repoName,
		Platform: *platform, Upstreams: warmUpstreams(cfg),
	}
	plan, err := warm.Build(opts)
	if err != nil {
		fmt.Fprintf(stderr, "warm: %v\n", err)
		return exitUsage
	}
	for _, warning := range plan.Warnings {
		fmt.Fprintln(stderr, "warning: "+warning)
	}
	for _, skipped := range plan.Skipped {
		fmt.Fprintln(stderr, "skipped: "+skipped)
	}
	mounts := warmMounts(repos)
	token := ""
	ctx := context.Background()
	if !*dryRun {
		token = ""
	}
	httpClient := &http.Client{Timeout: 10 * time.Minute}
	total := estimatePlan(ctx, httpClient, base, mounts, plan.Items)
	if *maxBytes != "" {
		limit, err := config.ParseSize(*maxBytes)
		if err != nil {
			fmt.Fprintf(stderr, "max-bytes: %v\n", err)
			return exitUsage
		}
		if total > limit.Bytes() {
			printWarmPlan(stdout, plan, total)
			fmt.Fprintf(stderr, "plan needs %d bytes, above --max-bytes %d\n", total, limit.Bytes())
			return exitUsage
		}
	}
	stats, err := client.Stats(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return serverUnreachable(err)
	}
	budget := int64From(stats["budget_bytes"])
	used := int64From(stats["used_bytes"])
	fits := true
	if budget > 0 && total > 0 {
		limit := int64(cfg.Storage.HighWatermark * float64(budget))
		fits = used+total <= limit
		if !fits && !*force {
			printWarmPlan(stdout, plan, total)
			fmt.Fprintf(stderr, "plan would cross the high watermark (%d + %d > %d)\n", used, total, limit)
			return exitUsage
		}
	}
	if *dryRun {
		if *flags.json {
			return writeJSONOutput(stdout, map[string]any{"plan": plan, "total_bytes": total, "fits_budget": fits}, stderr)
		}
		fmt.Fprintf(stdout, "total known bytes: %d\n", total)
		fmt.Fprintf(stdout, "fits budget: %t\n", fits)
		return exitOK
	}
	if *pinSet != "" {
		token, err = client.CaptureStart(ctx, *pinSet, localCIDRs())
		if err != nil {
			fmt.Fprintf(stderr, "start capture: %v\n", err)
			return exitFailure
		}
	}
	summary := warm.Execute(ctx, plan, warm.ExecuteOptions{
		BaseURL: base, Mounts: mounts, Concurrency: *concurrency,
		Retries: *retries, FailFast: *failFast, Token: token, Client: httpClient,
	})
	if *pinSet != "" && token != "" {
		_, _, pinned, err := client.CaptureStop(ctx, token, true)
		if err != nil {
			fmt.Fprintf(stderr, "stop capture: %v\n", err)
			summary.Failed++
		} else {
			summary.Pinned = pinned
		}
	}
	if *flags.json {
		return finishWarmJSON(stdout, stderr, summary)
	}
	printWarmSummary(stdout, summary)
	if summary.Failed > 0 {
		return exitFailure
	}
	return exitOK
}

func finishWarmJSON(stdout, stderr io.Writer, summary warm.Summary) int {
	if code := writeJSONOutput(stdout, summary, stderr); code != exitOK {
		return code
	}
	if summary.Failed > 0 {
		return exitFailure
	}
	return exitOK
}

func printWarmPlan(stdout io.Writer, plan warm.Plan, total int64) {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPO\tPATH\tSIZE\tKIND")
	for _, item := range plan.Items {
		kind := "artifact"
		if item.Metadata {
			kind = "metadata"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", item.Repo, item.Path, item.Size, kind)
	}
	_ = tw.Flush()
	fmt.Fprintf(stdout, "items: %d, known bytes: %d\n", len(plan.Items), total)
}

func printWarmSummary(stdout io.Writer, summary warm.Summary) {
	fmt.Fprintf(stdout, "planned: %d\n", summary.Planned)
	fmt.Fprintf(stdout, "cached: %d\n", summary.Cached)
	fmt.Fprintf(stdout, "fetched: %d\n", summary.Fetched)
	fmt.Fprintf(stdout, "failed: %d\n", summary.Failed)
	fmt.Fprintf(stdout, "bytes fetched: %d\n", summary.Bytes)
	if summary.Pinned > 0 {
		fmt.Fprintf(stdout, "pinned: %d\n", summary.Pinned)
	}
	fmt.Fprintf(stdout, "elapsed: %s\n", (time.Duration(summary.ElapsedMS) * time.Millisecond).Round(time.Millisecond))
	for _, result := range summary.Results {
		if result.Error != "" {
			fmt.Fprintf(stdout, "failed: %s %s: %s\n", result.Repo, result.Path, result.Error)
		}
	}
}

func estimatePlan(ctx context.Context, client *http.Client, base string, mounts map[string]string, items []warm.Item) int64 {
	total := int64(0)
	for index := range items {
		if items[index].Size > 0 {
			total += items[index].Size
			continue
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodHead, warmURL(base, mounts[items[index].Repo], items[index].Path), nil)
		if err != nil {
			continue
		}
		response, err := client.Do(request)
		if err != nil {
			continue
		}
		_ = response.Body.Close()
		if response.StatusCode == http.StatusOK {
			if size, err := strconv.ParseInt(response.Header.Get("Content-Length"), 10, 64); err == nil && size > 0 {
				total += size
			}
		}
	}
	return total
}

func warmURL(base, mount, path string) string {
	base = strings.TrimSuffix(base, "/")
	mount = strings.Trim(mount, "/")
	path = strings.TrimPrefix(path, "/")
	parts := make([]string, 0, 3)
	if mount != "" {
		parts = append(parts, mount)
	}
	if path != "" {
		parts = append(parts, path)
	}
	if len(parts) == 0 {
		return base + "/"
	}
	return base + "/" + strings.Join(parts, "/")
}

func warmRepos(cfg *config.Config) []warm.Repo {
	repos := make([]warm.Repo, 0, len(cfg.Repos))
	for _, repo := range cfg.Repos {
		repos = append(repos, warm.Repo{Name: repo.Name, Kind: repo.Kind, Mount: repo.Mount})
	}
	return repos
}

func warmUpstreams(cfg *config.Config) []warm.RepoUpstream {
	var out []warm.RepoUpstream
	for _, repo := range cfg.Repos {
		for _, upstream := range repo.Upstreams {
			out = append(out, warm.RepoUpstream{Repo: repo.Name, URL: upstream.URL})
		}
	}
	return out
}

func warmMounts(repos []warm.Repo) map[string]string {
	mounts := make(map[string]string, len(repos))
	for _, repo := range repos {
		mounts[repo.Name] = repo.Mount
	}
	return mounts
}

func parseRepoMap(value string) map[string]string {
	out := make(map[string]string)
	for _, part := range splitCSV(value) {
		kind, name, ok := strings.Cut(part, "=")
		if ok && kind != "" && name != "" {
			out[kind] = name
		}
	}
	return out
}

func splitCSV(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func int64From(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case json.Number:
		number, _ := typed.Int64()
		return number
	default:
		return 0
	}
}

func localCIDRs() []string {
	out := []string{"127.0.0.1/32", "::1/128"}
	interfaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range interfaces {
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err != nil {
				continue
			}
			if ip.IsLoopback() {
				continue
			}
			if ip.To4() != nil {
				out = append(out, ip.String()+"/32")
			} else {
				out = append(out, ip.String()+"/128")
			}
		}
	}
	return out
}
