package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mmrzaf/larder/internal/admin"
	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/storage"
	"github.com/mmrzaf/larder/internal/store"
)

func runStorage(args []string, configPath, adminURL string, jsonDefault bool, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "storage command is required")
		return exitUsage
	}
	command := args[0]
	rest := args[1:]
	switch command {
	case "stats":
		return runStats(rest, configPath, adminURL, jsonDefault, stdout, stderr)
	case "du":
		return runDU(rest, configPath, jsonDefault, stdout, stderr)
	case "ls":
		return runList(rest, configPath, jsonDefault, stdout, stderr)
	case "objects":
		return runObjects(rest, configPath, jsonDefault, stdout, stderr)
	case "rollback":
		return runRollback(rest, configPath, adminURL, jsonDefault, stdout, stderr)
	case "pin":
		return runPin(rest, configPath, adminURL, jsonDefault, stdout, stderr)
	case "unpin":
		return runUnpin(rest, configPath, adminURL, jsonDefault, stdout, stderr)
	case "pins":
		return runPins(rest, configPath, jsonDefault, stdout, stderr)
	case "gc":
		return runGC(rest, configPath, adminURL, jsonDefault, stdout, stderr)
	case "evict":
		return runEvict(rest, configPath, adminURL, jsonDefault, stdout, stderr)
	case "prune":
		return runPrune(rest, configPath, adminURL, jsonDefault, stdout, stderr)
	case "offline":
		return runOffline(rest, configPath, adminURL, jsonDefault, stdout, stderr)
	case "reload":
		return runReload(rest, configPath, adminURL, jsonDefault, stdout, stderr)
	case "fetch":
		return runFetch(rest, configPath, adminURL, jsonDefault, stdout, stderr)
	case "verify":
		return runVerify(rest, configPath, adminURL, jsonDefault, stdout, stderr)
	case "reindex":
		return runReindex(rest, configPath, jsonDefault, stdout, stderr)
	case "snapshot":
		return runSnapshot(rest, configPath, jsonDefault, stdout, stderr)
	case "warm":
		return runWarm(rest, configPath, adminURL, jsonDefault, stdout, stderr)
	case "seed":
		return runSeed(rest, configPath, adminURL, jsonDefault, stdout, stderr)
	case "export":
		return runExport(rest, configPath, jsonDefault, stdout, stderr)
	case "import":
		return runImport(rest, configPath, jsonDefault, stdout, stderr)
	case "ingest":
		return runIngest(rest, configPath, jsonDefault, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown storage command %q\n", command)
		return exitUsage
	}
}

type storageFlags struct {
	fs     *flag.FlagSet
	json   *bool
	admin  *string
	config *string
}

func newStorageFlags(name, configPath, adminURL string, jsonDefault bool, stderr io.Writer) *storageFlags {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	config := fs.String("config", configPath, "configuration file")
	fs.StringVar(config, "c", configPath, "configuration file")
	return &storageFlags{
		fs:     fs,
		json:   fs.Bool("json", jsonDefault, "machine-readable output"),
		admin:  fs.String("admin", adminURL, "admin API URL"),
		config: config,
	}
}

func (f *storageFlags) parse(args []string) bool {
	return f.fs.Parse(interspersed(args, f.fs)) == nil
}

// interspersed moves flags ahead of positional arguments so the command forms
// shown in the documentation work regardless of flag placement.
func interspersed(args []string, fs *flag.FlagSet) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			positional = append(positional, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if index := strings.IndexByte(name, '='); index >= 0 {
			flags = append(flags, arg)
			continue
		}
		definition := fs.Lookup(name)
		if definition == nil {
			flags = append(flags, arg)
			continue
		}
		flags = append(flags, arg)
		if boolFlag, ok := definition.Value.(interface{ IsBoolFlag() bool }); ok && boolFlag.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

func loadStorageConfig(path string, stderr io.Writer) (*config.Config, int) {
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return nil, exitUsage
	}
	return cfg, exitOK
}

func writeJSONOutput(w io.Writer, value any, stderr io.Writer) int {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintf(stderr, "write output: %v\n", err)
		return exitFailure
	}
	return exitOK
}

func openReadOnlyManager(cfg *config.Config) (*storage.Manager, func(), error) {
	st, err := store.OpenReadOnly(cfg.DataDir)
	if err != nil {
		return nil, nil, err
	}
	idx, err := index.OpenReadOnly(cfg.DataDir + "/index.db")
	if err != nil {
		_ = st.Close()
		return nil, nil, err
	}
	manager, err := storage.New(storage.Options{Config: cfg, Store: st, Index: idx})
	if err != nil {
		_ = idx.Close()
		_ = st.Close()
		return nil, nil, err
	}
	return manager, func() {
		_ = idx.Close()
		_ = st.Close()
	}, nil
}

func openDirectManager(cfg *config.Config) (*storage.Manager, func(), error) {
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, nil, err
	}
	idx, err := index.Open(cfg.DataDir + "/index.db")
	if err != nil {
		_ = st.Close()
		return nil, nil, err
	}
	manager, err := storage.New(storage.Options{Config: cfg, Store: st, Index: idx})
	if err != nil {
		_ = idx.Close()
		_ = st.Close()
		return nil, nil, err
	}
	return manager, func() {
		_ = idx.Close()
		_ = st.Close()
	}, nil
}

func adminClient(cfg *config.Config, override string) (*admin.Client, string, error) {
	base := strings.TrimSpace(override)
	if base == "" {
		if cfg.Admin.Listen == "" {
			return nil, "", nil
		}
		base = adminURLFromListen(cfg.Admin.Listen)
	}
	client, err := admin.New(base, 30*time.Second)
	if err != nil {
		return nil, "", err
	}
	return client, base, nil
}

func adminURLFromListen(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		if strings.HasPrefix(listen, ":") {
			host, port = "", strings.TrimPrefix(listen, ":")
		} else {
			return "http://" + listen
		}
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

type statsDocument struct {
	BudgetBytes      int64                `json:"budget_bytes"`
	UsedBytes        int64                `json:"used_bytes"`
	BlobBytes        int64                `json:"blob_bytes"`
	IndexBytes       int64                `json:"index_bytes"`
	SnapshotBytes    int64                `json:"snapshot_bytes"`
	PinnedBytes      int64                `json:"pinned_bytes"`
	FreeBytes        int64                `json:"disk_free_bytes"`
	CapacityBytes    int64                `json:"disk_capacity_bytes"`
	Blobs            int64                `json:"blobs"`
	Objects          int64                `json:"objects"`
	PinnedOverBudget bool                 `json:"pinned_over_budget"`
	BypassActive     bool                 `json:"bypass_active"`
	Offline          bool                 `json:"offline"`
	RequestsTotal    *int64               `json:"requests_total,omitempty"`
	HitsTotal        *int64               `json:"hits_total,omitempty"`
	HitRatio         *float64             `json:"hit_ratio,omitempty"`
	ByClass          []storage.ClassTotal `json:"by_class,omitempty"`
	Repos            []storage.RepoTotal  `json:"repos,omitempty"`
}

func runStats(args []string, configPath, adminURL string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("stats", configPath, adminURL, jsonDefault, stderr)
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() != 0 {
		fmt.Fprintln(stderr, "stats does not accept arguments")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	var document statsDocument
	if client, _, err := adminClient(cfg, *flags.admin); err == nil && client != nil {
		if err := client.Get(context.Background(), "/_larder/v1/stats", &document); err == nil {
			return printStats(stdout, stderr, document, *flags.json)
		} else if !errors.Is(err, admin.ErrUnreachable) {
			fmt.Fprintln(stderr, err)
			return exitFailure
		}
	}
	manager, closeManager, err := openReadOnlyManager(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "open storage: %v\n", err)
		return exitFailure
	}
	defer closeManager()
	stats, err := manager.Stats(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "read stats: %v\n", err)
		return exitFailure
	}
	document = statsDocument{
		BudgetBytes: stats.BudgetBytes, UsedBytes: stats.UsedBytes,
		BlobBytes: stats.BlobBytes, IndexBytes: stats.IndexBytes,
		SnapshotBytes: stats.SnapshotBytes, PinnedBytes: stats.PinnedBytes,
		FreeBytes: stats.FreeBytes, CapacityBytes: stats.CapacityBytes,
		Blobs: stats.Blobs, Objects: stats.Objects,
		PinnedOverBudget: manager.PinnedOverBudget(), BypassActive: manager.BypassActive(),
		Offline: cfg.Offline, ByClass: stats.ByClass, Repos: stats.ByRepo,
	}
	return printStats(stdout, stderr, document, *flags.json)
}

func printStats(stdout, stderr io.Writer, document statsDocument, asJSON bool) int {
	if asJSON {
		return writeJSONOutput(stdout, document, stderr)
	}
	fmt.Fprintf(stdout, "budget: %s\n", config.Size(document.BudgetBytes).String())
	fmt.Fprintf(stdout, "used: %s\n", config.Size(document.UsedBytes).String())
	fmt.Fprintf(stdout, "free disk: %s\n", config.Size(document.FreeBytes).String())
	fmt.Fprintf(stdout, "pinned: %s\n", config.Size(document.PinnedBytes).String())
	fmt.Fprintf(stdout, "objects: %d\n", document.Objects)
	fmt.Fprintf(stdout, "blobs: %d\n", document.Blobs)
	if document.RequestsTotal != nil {
		fmt.Fprintf(stdout, "requests: %d\n", *document.RequestsTotal)
		if document.HitRatio != nil {
			fmt.Fprintf(stdout, "hit ratio: %.3f\n", *document.HitRatio)
		}
	}
	fmt.Fprintf(stdout, "offline: %t\n", document.Offline)
	fmt.Fprintf(stdout, "bypass: %t\n", document.BypassActive)
	fmt.Fprintf(stdout, "pinned over budget: %t\n", document.PinnedOverBudget)
	if len(document.Repos) > 0 {
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "REPO\tOBJECTS\tBLOBS\tBYTES")
		for _, repo := range document.Repos {
			fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n", repo.Repo, repo.Objects, repo.Blobs, config.Size(repo.StoredBytes).String())
		}
		_ = tw.Flush()
	}
	return exitOK
}

func runDU(args []string, configPath string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("du", configPath, "", jsonDefault, stderr)
	by := flags.fs.String("by", "repo", "repo, class, age or top")
	n := flags.fs.Int("n", 50, "number of objects for top")
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() != 0 {
		fmt.Fprintln(stderr, "du does not accept arguments")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	manager, closeManager, err := openReadOnlyManager(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "open storage: %v\n", err)
		return exitFailure
	}
	defer closeManager()
	result, err := manager.DU(context.Background(), *by, *n)
	if err != nil {
		fmt.Fprintf(stderr, "du: %v\n", err)
		return exitUsage
	}
	if *flags.json {
		return writeJSONOutput(stdout, result, stderr)
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	switch *by {
	case "repo":
		fmt.Fprintln(tw, "REPO\tOBJECTS\tBLOBS\tBYTES")
		for _, repo := range result.Repos {
			fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n", repo.Repo, repo.Objects, repo.Blobs, config.Size(repo.StoredBytes).String())
		}
	case "class":
		fmt.Fprintln(tw, "REPO\tCLASS\tOBJECTS\tBLOBS\tBYTES")
		for _, class := range result.Classes {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\n", class.Repo, class.Class, class.Objects,
				class.Blobs, config.Size(class.StoredBytes).String())
		}
	case "age":
		fmt.Fprintln(tw, "AGE_DAYS\tOBJECTS\tBYTES")
		for _, bucket := range result.Ages {
			fmt.Fprintf(tw, "%s\t%d\t%s\n", bucket.Label, bucket.Objects, config.Size(bucket.Bytes).String())
		}
	case "top":
		fmt.Fprintln(tw, "REPO\tKEY\tBYTES\tLAST_ACCESS")
		for _, object := range result.Top {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", object.Repo, object.Key,
				config.Size(object.Size).String(), object.LastAccess.Format(time.RFC3339))
		}
	}
	_ = tw.Flush()
	return exitOK
}

func runList(args []string, configPath string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("ls", configPath, "", jsonDefault, stderr)
	sortBy := flags.fs.String("sort", "", "size, atime or hits")
	limit := flags.fs.Int("limit", 0, "maximum objects")
	pinned := flags.fs.Bool("pinned", false, "only pinned objects")
	unverified := flags.fs.Bool("unverified", false, "only unverified objects")
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() < 1 || flags.fs.NArg() > 2 {
		fmt.Fprintln(stderr, "usage: larder ls <repo> [glob]")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	options := storage.ListOptions{
		Repo: flags.fs.Arg(0), Sort: *sortBy, Limit: *limit,
		PinnedOnly: *pinned, UnverifiedOnly: *unverified,
	}
	if flags.fs.NArg() == 2 {
		options.Match = flags.fs.Arg(1)
	}
	manager, closeManager, err := openReadOnlyManager(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "open storage: %v\n", err)
		return exitFailure
	}
	defer closeManager()
	objects, err := manager.List(context.Background(), options)
	if err != nil {
		fmt.Fprintf(stderr, "ls: %v\n", err)
		return exitFailure
	}
	if *flags.json {
		return writeJSONOutput(stdout, objects, stderr)
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPO\tKEY\tSIZE\tCLASS\tVERIFIED\tPINNED\tLAST_ACCESS\tHITS")
	for _, object := range objects {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%t\t%t\t%s\t%d\n", object.Repo, object.Key,
			config.Size(object.Size).String(), object.Class, object.Verified, object.Pinned,
			object.LastAccess.Format(time.RFC3339), object.Hits)
	}
	_ = tw.Flush()
	return exitOK
}

func runObjects(args []string, configPath string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("objects", configPath, "", jsonDefault, stderr)
	history := flags.fs.Bool("history", false, "include every generation")
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: larder objects <repo> <key>")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	manager, closeManager, err := openReadOnlyManager(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "open storage: %v\n", err)
		return exitFailure
	}
	defer closeManager()
	details, err := manager.Details(context.Background(), flags.fs.Arg(0), flags.fs.Arg(1), *history)
	if err != nil {
		fmt.Fprintf(stderr, "objects: %v\n", err)
		return exitFailure
	}
	if *flags.json {
		return writeJSONOutput(stdout, details, stderr)
	}
	object := details.Object
	fmt.Fprintf(stdout, "repo: %s\nkey: %s\nclass: %s\ngeneration: %d\n",
		object.Repo, object.Key, object.Class, object.Generation)
	fmt.Fprintf(stdout, "sha256: %s\nsize: %s\nraw size: %s\nstored encoding: %s\n",
		object.SHA256, config.Size(object.Size).String(), config.Size(object.RawSize).String(), object.StoredEnc)
	fmt.Fprintf(stdout, "verified: %t\npinned: %t\nlast access: %s\nhits: %d\n",
		object.Verified, object.Pinned, object.LastAccess.Format(time.RFC3339), object.Hits)
	fmt.Fprintf(stdout, "blob refcount: %d\n", details.Blob.RefCount)
	if len(details.Pins) > 0 {
		fmt.Fprintf(stdout, "pin sets: %s\n", strings.Join(details.Pins, ", "))
	}
	for _, expectation := range details.Expectations {
		fmt.Fprintf(stdout, "expectation: %s %s from %s\n", expectation.Algo, expectation.Digest, expectation.Source)
	}
	if *history {
		for _, generation := range details.Generations {
			fmt.Fprintf(stdout, "generation %d: %s verified=%t\n", generation.Generation,
				generation.SHA256, generation.Verified)
		}
	}
	return exitOK
}

func runRollback(args []string, configPath, adminURL string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("rollback", configPath, adminURL, jsonDefault, stderr)
	generation := flags.fs.Int64("generation", 0, "generation to restore")
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: larder rollback <repo> <key> [--generation N]")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	repo, key := flags.fs.Arg(0), flags.fs.Arg(1)
	client, _, err := adminClient(cfg, *flags.admin)
	if err == nil && client != nil {
		var result map[string]any
		err := client.Post(context.Background(), "/_larder/v1/rollback", map[string]any{
			"repo": repo, "key": key, "generation": *generation,
		}, &result)
		if err == nil {
			if *flags.json {
				return writeJSONOutput(stdout, result, stderr)
			}
			fmt.Fprintf(stdout, "rolled back %s/%s to generation %v\n", repo, key, result["generation"])
			return exitOK
		}
		if !errors.Is(err, admin.ErrUnreachable) {
			fmt.Fprintln(stderr, err)
			return exitFailure
		}
	}
	manager, closeManager, err := openDirectManager(cfg)
	if err != nil {
		return directStorageError(err, stderr)
	}
	defer closeManager()
	object, err := manager.Rollback(context.Background(), repo, key, *generation)
	if err != nil {
		fmt.Fprintf(stderr, "rollback: %v\n", err)
		return exitFailure
	}
	if *flags.json {
		return writeJSONOutput(stdout, object, stderr)
	}
	fmt.Fprintf(stdout, "rolled back %s/%s to generation %d\n", repo, key, object.Generation)
	return exitOK
}

func runPin(args []string, configPath, adminURL string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("pin", configPath, adminURL, jsonDefault, stderr)
	set := flags.fs.String("set", "", "pin set name")
	repo := flags.fs.String("repo", "", "repository")
	key := flags.fs.String("key", "", "exact key")
	match := flags.fs.String("match", "", "key glob")
	note := flags.fs.String("note", "", "set note")
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() != 0 {
		fmt.Fprintln(stderr, "pin does not accept positional arguments")
		return exitUsage
	}
	if *set == "" || *repo == "" || (*key == "" && *match == "") || (*key != "" && *match != "") {
		fmt.Fprintln(stderr, "pin requires --set, --repo and exactly one of --key or --match")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	request := map[string]any{"set": *set, "repo": *repo, "note": *note}
	if *key != "" {
		request["key"] = *key
	} else {
		request["match"] = *match
	}
	if client, _, err := adminClient(cfg, *flags.admin); err == nil && client != nil {
		if err := client.Post(context.Background(), "/_larder/v1/pins", request, nil); err == nil {
			return printPinResult(stdout, stderr, *flags.json, "pin set "+*set+" updated")
		} else if !errors.Is(err, admin.ErrUnreachable) {
			fmt.Fprintln(stderr, err)
			return exitFailure
		}
	}
	manager, closeManager, err := openDirectManager(cfg)
	if err != nil {
		return directStorageError(err, stderr)
	}
	defer closeManager()
	if err := manager.AddPin(context.Background(), storage.AddPinRequest{
		Set: *set, Repo: *repo, Key: *key, Match: *match, Note: *note,
	}); err != nil {
		fmt.Fprintf(stderr, "pin: %v\n", err)
		return exitFailure
	}
	return printPinResult(stdout, stderr, *flags.json, "pin set "+*set+" updated")
}

func printPinResult(stdout, stderr io.Writer, asJSON bool, text string) int {
	if asJSON {
		return writeJSONOutput(stdout, map[string]any{"pinned": true, "message": text}, stderr)
	}
	fmt.Fprintln(stdout, text)
	return exitOK
}

func runUnpin(args []string, configPath, adminURL string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("unpin", configPath, adminURL, jsonDefault, stderr)
	set := flags.fs.String("set", "", "pin set name")
	repo := flags.fs.String("repo", "", "repository for a rule")
	match := flags.fs.String("match", "", "glob rule to remove")
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() != 0 || *set == "" {
		fmt.Fprintln(stderr, "unpin requires --set")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	if client, _, err := adminClient(cfg, *flags.admin); err == nil && client != nil {
		path := "/_larder/v1/pins/" + url.PathEscape(*set)
		query := url.Values{}
		if *repo != "" {
			query.Set("repo", *repo)
		}
		if *match != "" {
			query.Set("match", *match)
		}
		if encoded := query.Encode(); encoded != "" {
			path += "?" + encoded
		}
		if err := client.Delete(context.Background(), path, nil); err == nil {
			return printPinResult(stdout, stderr, *flags.json, "removed pin set "+*set)
		} else if !errors.Is(err, admin.ErrUnreachable) {
			fmt.Fprintln(stderr, err)
			return exitFailure
		}
	}
	manager, closeManager, err := openDirectManager(cfg)
	if err != nil {
		return directStorageError(err, stderr)
	}
	defer closeManager()
	if err := manager.RemovePin(context.Background(), *set, *repo, *match); err != nil {
		fmt.Fprintf(stderr, "unpin: %v\n", err)
		return exitFailure
	}
	return printPinResult(stdout, stderr, *flags.json, "removed pin set "+*set)
}

func runPins(args []string, configPath string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("pins", configPath, "", jsonDefault, stderr)
	set := flags.fs.String("set", "", "only this set")
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() != 0 {
		fmt.Fprintln(stderr, "pins does not accept arguments")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	manager, closeManager, err := openReadOnlyManager(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "open storage: %v\n", err)
		return exitFailure
	}
	defer closeManager()
	pins, err := manager.Pins(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "pins: %v\n", err)
		return exitFailure
	}
	if *set != "" {
		filtered := pins[:0]
		for _, pin := range pins {
			if pin.Name == *set {
				filtered = append(filtered, pin)
			}
		}
		pins = filtered
	}
	if *flags.json {
		return writeJSONOutput(stdout, pins, stderr)
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSOURCE\tITEMS\tRULES\tOBJECTS\tBYTES")
	for _, pin := range pins {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\n", pin.Name, pin.Source, pin.Items,
			pin.Rules, pin.MatchedObjects, config.Size(pin.Bytes).String())
	}
	_ = tw.Flush()
	return exitOK
}

func runGC(args []string, configPath, adminURL string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("gc", configPath, adminURL, jsonDefault, stderr)
	targetBytes := flags.fs.String("target-bytes", "", "evict until used is at most this size")
	targetPct := flags.fs.Float64("target-pct", 0, "evict until used is at most this percentage of the budget")
	policy := flags.fs.String("policy", "", "lru, lfu or score")
	dryRun := flags.fs.Bool("dry-run", false, "list victims without removing them")
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() != 0 {
		fmt.Fprintln(stderr, "gc does not accept arguments")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	options := storage.GCOptions{DryRun: *dryRun, Policy: *policy, Trigger: "manual"}
	if *targetBytes != "" {
		size, err := config.ParseSize(*targetBytes)
		if err != nil {
			fmt.Fprintf(stderr, "target-bytes: %v\n", err)
			return exitUsage
		}
		options.HasTarget = true
		options.TargetBytes = size.Bytes()
	}
	if *targetPct < 0 || *targetPct > 100 {
		fmt.Fprintln(stderr, "target-pct must be between 0 and 100")
		return exitUsage
	}
	switch *policy {
	case "", "lru", "lfu", "score":
	default:
		fmt.Fprintf(stderr, "unknown eviction policy %q\n", *policy)
		return exitUsage
	}
	options.TargetPct = *targetPct
	if client, _, err := adminClient(cfg, *flags.admin); err == nil && client != nil {
		body := map[string]any{"dry_run": *dryRun, "policy": *policy, "target_pct": *targetPct}
		if options.HasTarget {
			body["target_bytes"] = options.TargetBytes
		}
		var accepted struct {
			Job string `json:"job"`
		}
		err := client.Post(context.Background(), "/_larder/v1/gc", body, &accepted)
		if err == nil {
			var result storage.GCResult
			if err := waitJob(context.Background(), client, accepted.Job, &result); err != nil {
				fmt.Fprintf(stderr, "gc: %v\n", err)
				return exitFailure
			}
			return printGCResult(stdout, stderr, result, *flags.json)
		}
		if !errors.Is(err, admin.ErrUnreachable) {
			fmt.Fprintln(stderr, err)
			return exitFailure
		}
	}
	manager, closeManager, err := openDirectManager(cfg)
	if err != nil {
		return directStorageError(err, stderr)
	}
	defer closeManager()
	result, err := manager.GC(context.Background(), options)
	if err != nil {
		fmt.Fprintf(stderr, "gc: %v\n", err)
		return exitFailure
	}
	return printGCResult(stdout, stderr, result, *flags.json)
}

func waitJob(ctx context.Context, client *admin.Client, id string, out any) error {
	if id == "" {
		return fmt.Errorf("admin API returned no job id")
	}
	deadline := time.Now().Add(30 * time.Minute)
	for {
		var document struct {
			State  string          `json:"state"`
			Result json.RawMessage `json:"result"`
			Error  string          `json:"error"`
		}
		if err := client.Get(ctx, "/_larder/v1/jobs/"+id, &document); err != nil {
			return err
		}
		switch document.State {
		case "done":
			if out != nil && len(document.Result) > 0 {
				return json.Unmarshal(document.Result, out)
			}
			return nil
		case "failed":
			if document.Error == "" {
				document.Error = "job failed"
			}
			return errors.New(document.Error)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("job %s did not finish", id)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func printGCResult(stdout, stderr io.Writer, result storage.GCResult, asJSON bool) int {
	if asJSON {
		return writeJSONOutput(stdout, result, stderr)
	}
	fmt.Fprintf(stdout, "budget: %s\n", config.Size(result.BudgetBytes).String())
	fmt.Fprintf(stdout, "used: %s -> %s\n", config.Size(result.UsedBefore).String(), config.Size(result.UsedAfter).String())
	if result.DryRun {
		fmt.Fprintln(stdout, "dry run: nothing removed")
	}
	for _, tier := range result.Tiers {
		fmt.Fprintf(stdout, "tier %d: %d objects, %s\n", tier.Tier, tier.Objects, config.Size(tier.Bytes).String())
		if result.DryRun {
			for _, victim := range tier.Victims {
				name := victim.Key
				if name == "" {
					name = victim.Path
				}
				if victim.Repo != "" {
					name = victim.Repo + "/" + name
				}
				fmt.Fprintf(stdout, "  %s %s frees %s\n", victim.Kind, name, config.Size(victim.Freed).String())
			}
		}
	}
	if result.Stopped != "" {
		fmt.Fprintf(stdout, "stopped: %s\n", result.Stopped)
	}
	return exitOK
}

func runEvict(args []string, configPath, adminURL string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("evict", configPath, adminURL, jsonDefault, stderr)
	includePinned := flags.fs.Bool("include-pinned", false, "also remove pinned objects")
	olderThan := flags.fs.Int("older-than", 0, "only objects not accessed for this many days")
	largerThan := flags.fs.String("larger-than", "", "only objects larger than this size")
	dryRun := flags.fs.Bool("dry-run", false, "list victims without removing them")
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: larder evict <repo> <glob>")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	options := storage.EvictOptions{
		Repo: flags.fs.Arg(0), Glob: flags.fs.Arg(1), IncludePinned: *includePinned,
		OlderThanDays: *olderThan, DryRun: *dryRun,
	}
	if *largerThan != "" {
		size, err := config.ParseSize(*largerThan)
		if err != nil {
			fmt.Fprintf(stderr, "larger-than: %v\n", err)
			return exitUsage
		}
		options.LargerThan = size.Bytes()
	}
	if client, _, err := adminClient(cfg, *flags.admin); err == nil && client != nil {
		body := map[string]any{
			"repo": options.Repo, "glob": options.Glob, "include_pinned": options.IncludePinned,
			"older_than_days": options.OlderThanDays, "larger_than": options.LargerThan,
			"dry_run": options.DryRun,
		}
		var accepted struct {
			Job string `json:"job"`
		}
		err := client.Post(context.Background(), "/_larder/v1/evict", body, &accepted)
		if err == nil {
			var result storage.EvictResult
			if err := waitJob(context.Background(), client, accepted.Job, &result); err != nil {
				fmt.Fprintf(stderr, "evict: %v\n", err)
				return exitFailure
			}
			return printEvictResult(stdout, stderr, result, *flags.json)
		}
		if !errors.Is(err, admin.ErrUnreachable) {
			fmt.Fprintln(stderr, err)
			return exitFailure
		}
	}
	manager, closeManager, err := openDirectManager(cfg)
	if err != nil {
		return directStorageError(err, stderr)
	}
	defer closeManager()
	result, err := manager.Evict(context.Background(), options)
	if err != nil {
		fmt.Fprintf(stderr, "evict: %v\n", err)
		return exitFailure
	}
	return printEvictResult(stdout, stderr, result, *flags.json)
}

func printEvictResult(stdout, stderr io.Writer, result storage.EvictResult, asJSON bool) int {
	if asJSON {
		return writeJSONOutput(stdout, result, stderr)
	}
	fmt.Fprintf(stdout, "%s %s: %d objects, %s\n", result.Repo, result.Glob,
		result.Objects, config.Size(result.Bytes).String())
	if result.DryRun {
		fmt.Fprintln(stdout, "dry run: nothing removed")
		for _, victim := range result.Victims {
			fmt.Fprintf(stdout, "  %s generation %d frees %s\n", victim.Key, victim.Generation,
				config.Size(victim.Freed).String())
		}
	}
	return exitOK
}

func runPrune(args []string, configPath, adminURL string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("prune", configPath, adminURL, jsonDefault, stderr)
	history := flags.fs.Bool("history", false, "prune metadata history")
	keep := flags.fs.Int("keep", -1, "generations to keep")
	repo := flags.fs.String("repo", "", "only this repository")
	dryRun := flags.fs.Bool("dry-run", false, "list victims without removing them")
	if !flags.parse(args) {
		return exitUsage
	}
	if !*history {
		fmt.Fprintln(stderr, "prune requires --history")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	if client, _, err := adminClient(cfg, *flags.admin); err == nil && client != nil {
		body := map[string]any{"history": true, "repo": *repo, "dry_run": *dryRun}
		if *keep >= 0 {
			body["keep"] = *keep
		}
		var result storage.PruneResult
		err := client.Post(context.Background(), "/_larder/v1/prune", body, &result)
		if err == nil {
			return printPruneResult(stdout, stderr, result, *flags.json)
		}
		if !errors.Is(err, admin.ErrUnreachable) {
			fmt.Fprintln(stderr, err)
			return exitFailure
		}
	}
	manager, closeManager, err := openDirectManager(cfg)
	if err != nil {
		return directStorageError(err, stderr)
	}
	defer closeManager()
	result, err := manager.Prune(context.Background(), storage.PruneOptions{
		Keep: *keep, Repo: *repo, DryRun: *dryRun,
	})
	if err != nil {
		fmt.Fprintf(stderr, "prune: %v\n", err)
		return exitFailure
	}
	return printPruneResult(stdout, stderr, result, *flags.json)
}

func printPruneResult(stdout, stderr io.Writer, result storage.PruneResult, asJSON bool) int {
	if asJSON {
		return writeJSONOutput(stdout, result, stderr)
	}
	fmt.Fprintf(stdout, "keep %d: %d objects, %s\n", result.Keep, result.Objects,
		config.Size(result.Bytes).String())
	if result.DryRun {
		fmt.Fprintln(stdout, "dry run: nothing removed")
		for _, victim := range result.Victims {
			fmt.Fprintf(stdout, "  %s/%s generation %d frees %s\n", victim.Repo, victim.Key,
				victim.Generation, config.Size(victim.Freed).String())
		}
	}
	return exitOK
}

func runOffline(args []string, configPath, adminURL string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("offline", configPath, adminURL, jsonDefault, stderr)
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: larder offline on|off|status")
		return exitUsage
	}
	action := flags.fs.Arg(0)
	if action != "on" && action != "off" && action != "status" {
		fmt.Fprintln(stderr, "offline takes on, off or status")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	client, _, err := adminClient(cfg, *flags.admin)
	if err != nil || client == nil {
		fmt.Fprintln(stderr, "admin API is not configured")
		return 3
	}
	if action == "status" {
		var document struct {
			Offline bool `json:"offline"`
		}
		if err := client.Get(context.Background(), "/_larder/v1/stats", &document); err != nil {
			fmt.Fprintln(stderr, err)
			return serverUnreachable(err)
		}
		if *flags.json {
			return writeJSONOutput(stdout, map[string]bool{"offline": document.Offline}, stderr)
		}
		fmt.Fprintf(stdout, "offline: %t\n", document.Offline)
		return exitOK
	}
	if err := client.SetOffline(context.Background(), action == "on"); err != nil {
		fmt.Fprintln(stderr, err)
		return serverUnreachable(err)
	}
	enabled := action == "on"
	if *flags.json {
		return writeJSONOutput(stdout, map[string]bool{"offline": enabled}, stderr)
	}
	fmt.Fprintf(stdout, "offline: %t\n", enabled)
	return exitOK
}

func runReload(args []string, configPath, adminURL string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("reload", configPath, adminURL, jsonDefault, stderr)
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() != 0 {
		fmt.Fprintln(stderr, "reload does not accept arguments")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	client, _, err := adminClient(cfg, *flags.admin)
	if err != nil || client == nil {
		fmt.Fprintln(stderr, "admin API is not configured")
		return 3
	}
	if err := client.Reload(context.Background()); err != nil {
		fmt.Fprintln(stderr, err)
		return serverUnreachable(err)
	}
	if *flags.json {
		return writeJSONOutput(stdout, map[string]bool{"reloaded": true}, stderr)
	}
	fmt.Fprintln(stdout, "configuration reloaded")
	return exitOK
}

type fetchResult struct {
	Repo   string `json:"repo"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	Cache  string `json:"cache"`
	Bytes  int64  `json:"bytes"`
	Key    string `json:"key,omitempty"`
}

func runFetch(args []string, configPath, adminURL string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("fetch", configPath, adminURL, jsonDefault, stderr)
	fromStdin := flags.fs.Bool("stdin", false, "read paths from standard input")
	pinSet := flags.fs.String("pin-set", "", "pin fetched keys into this set")
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() < 1 {
		fmt.Fprintln(stderr, "usage: larder fetch <repo> <path>...")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	repoName := flags.fs.Arg(0)
	repo, ok := findRepo(cfg, repoName)
	if !ok {
		fmt.Fprintf(stderr, "unknown repo %q\n", repoName)
		return exitUsage
	}
	paths := append([]string(nil), flags.fs.Args()[1:]...)
	if *fromStdin {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			value := strings.TrimSpace(scanner.Text())
			if value != "" {
				paths = append(paths, value)
			}
		}
		if err := scanner.Err(); err != nil {
			fmt.Fprintf(stderr, "read standard input: %v\n", err)
			return exitFailure
		}
	}
	if len(paths) == 0 {
		fmt.Fprintln(stderr, "no paths to fetch")
		return exitUsage
	}
	base, err := publicBase(cfg, repo)
	if err != nil {
		fmt.Fprintf(stderr, "public URL: %v\n", err)
		return exitUsage
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	results := make([]fetchResult, 0, len(paths))
	failures := 0
	var pinRequests []storage.AddPinRequest
	for _, pathValue := range paths {
		target := base + joinMount(repo.Mount, pathValue)
		response, err := client.Get(target)
		if err != nil {
			fmt.Fprintf(stderr, "fetch %s: %v\n", target, err)
			return 3
		}
		written, readErr := io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			fmt.Fprintf(stderr, "fetch %s: %v\n", target, readErr)
			return exitFailure
		}
		result := fetchResult{
			Repo: repoName, Path: pathValue, Status: response.StatusCode,
			Cache: response.Header.Get("X-Larder-Cache"), Bytes: written,
			Key: response.Header.Get("X-Larder-Key"),
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			failures++
		}
		results = append(results, result)
		if *pinSet != "" && result.Key != "" && result.Status >= 200 && result.Status < 300 {
			keyRepo, key, ok := strings.Cut(result.Key, ":")
			if ok && keyRepo == repoName {
				pinRequests = append(pinRequests, storage.AddPinRequest{
					Set: *pinSet, Repo: keyRepo, Key: key,
				})
			}
		}
	}
	if *flags.json {
		if code := writeJSONOutput(stdout, results, stderr); code != exitOK {
			return code
		}
	} else {
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "PATH\tSTATUS\tCACHE\tBYTES")
		for _, result := range results {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%d\n", result.Path, result.Status, result.Cache, result.Bytes)
		}
		_ = tw.Flush()
	}
	if len(pinRequests) > 0 {
		manager, closeManager, err := openDirectManager(cfg)
		if err != nil {
			if client, _, clientErr := adminClient(cfg, *flags.admin); clientErr == nil && client != nil {
				for _, request := range pinRequests {
					body := map[string]any{"set": request.Set, "repo": request.Repo, "key": request.Key}
					if err := client.Post(context.Background(), "/_larder/v1/pins", body, nil); err != nil {
						fmt.Fprintf(stderr, "pin %s: %v\n", request.Key, err)
						return exitFailure
					}
				}
			} else {
				return directStorageError(err, stderr)
			}
		} else {
			defer closeManager()
			for _, request := range pinRequests {
				if err := manager.AddPin(context.Background(), request); err != nil {
					fmt.Fprintf(stderr, "pin %s: %v\n", request.Key, err)
					return exitFailure
				}
			}
		}
	}
	if failures > 0 {
		return exitFailure
	}
	return exitOK
}

func findRepo(cfg *config.Config, name string) (config.Repo, bool) {
	for _, repo := range cfg.Repos {
		if repo.Name == name {
			return repo, true
		}
	}
	return config.Repo{}, false
}

func publicBase(cfg *config.Config, repo config.Repo) (string, error) {
	base := cfg.PublicBaseURL
	if repo.PublicURL != "" {
		base = repo.PublicURL
	}
	if base == "" {
		host, port, err := net.SplitHostPort(cfg.Listen)
		if err != nil {
			return "", fmt.Errorf("parse listen address %q: %w", cfg.Listen, err)
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		base = "http://" + net.JoinHostPort(host, port)
	}
	return strings.TrimSuffix(base, "/"), nil
}

func joinMount(mount, pathValue string) string {
	if mount == "" {
		mount = "/"
	}
	if !strings.HasPrefix(mount, "/") {
		mount = "/" + mount
	}
	pathValue = strings.TrimPrefix(pathValue, "/")
	if pathValue == "" {
		return strings.TrimSuffix(mount, "/")
	}
	return strings.TrimSuffix(mount, "/") + "/" + pathValue
}

func directStorageError(err error, stderr io.Writer) int {
	if errors.Is(err, store.ErrLocked) {
		fmt.Fprintln(stderr, "data directory is locked by a running server")
		return 4
	}
	fmt.Fprintf(stderr, "open storage: %v\n", err)
	return exitFailure
}

func serverUnreachable(err error) int {
	if errors.Is(err, admin.ErrUnreachable) {
		return 3
	}
	return exitFailure
}
