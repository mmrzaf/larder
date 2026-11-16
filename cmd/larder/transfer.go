package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/mmrzaf/larder/internal/config"
	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/kinds"
	"github.com/mmrzaf/larder/internal/kinds/alpinekind"
	"github.com/mmrzaf/larder/internal/kinds/aptkind"
	"github.com/mmrzaf/larder/internal/kinds/cratekind"
	"github.com/mmrzaf/larder/internal/kinds/generickind"
	"github.com/mmrzaf/larder/internal/kinds/gokind"
	"github.com/mmrzaf/larder/internal/kinds/mavenkind"
	"github.com/mmrzaf/larder/internal/kinds/npmkind"
	"github.com/mmrzaf/larder/internal/kinds/pypikind"
	"github.com/mmrzaf/larder/internal/kinds/rustupkind"
	"github.com/mmrzaf/larder/internal/storage"
	"github.com/mmrzaf/larder/internal/store"
	"github.com/mmrzaf/larder/internal/transfer"
)

func runExport(args []string, configPath string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("export", configPath, "", jsonDefault, stderr)
	to := flags.fs.String("to", "", "destination directory or archive")
	repo := flags.fs.String("repo", "", "only this repository")
	pinned := flags.fs.Bool("pinned", false, "only pinned objects")
	match := flags.fs.String("match", "", "key glob")
	older := flags.fs.Int("older-than-days", 0, "only objects not accessed for this many days")
	baseURL := flags.fs.String("base-url", "", "public base URL for rewritten documents")
	format := flags.fs.String("format", "tree", "tree or tar")
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() != 0 || *to == "" {
		fmt.Fprintln(stderr, "export requires --to")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	st, err := store.OpenReadOnly(cfg.DataDir)
	if err != nil {
		fmt.Fprintf(stderr, "open storage: %v\n", err)
		return exitFailure
	}
	defer func() { _ = st.Close() }()
	live, err := index.OpenReadOnly(filepath.Join(cfg.DataDir, "index.db"))
	if err != nil {
		fmt.Fprintf(stderr, "open index: %v\n", err)
		return exitFailure
	}
	snapshotDir := filepath.Join(cfg.DataDir, "snapshots")
	if err := os.MkdirAll(snapshotDir, 0o750); err != nil {
		_ = live.Close()
		fmt.Fprintf(stderr, "create snapshot directory: %v\n", err)
		return exitFailure
	}
	temp, err := os.CreateTemp(snapshotDir, ".export-*.db")
	if err != nil {
		_ = live.Close()
		fmt.Fprintf(stderr, "create snapshot: %v\n", err)
		return exitFailure
	}
	snapshotPath := temp.Name()
	_ = temp.Close()
	_ = os.Remove(snapshotPath)
	if err := live.VacuumInto(context.Background(), snapshotPath); err != nil {
		_ = live.Close()
		fmt.Fprintf(stderr, "snapshot index: %v\n", err)
		return exitFailure
	}
	_ = live.Close()
	defer func() { _ = removeFile(snapshotPath) }()
	idx, err := index.OpenReadOnly(snapshotPath)
	if err != nil {
		fmt.Fprintf(stderr, "open snapshot: %v\n", err)
		return exitFailure
	}
	defer func() { _ = idx.Close() }()
	manager, err := storage.New(storage.Options{Config: cfg, Store: st, Index: idx})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailure
	}
	registry, err := transferRegistry()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailure
	}
	result, err := transfer.Export(context.Background(), st, idx, cfg, registry, transfer.ExportOptions{
		To: *to, Format: *format, Repo: *repo, Match: *match, Pinned: *pinned,
		OlderThanDays: *older, BaseURL: *baseURL,
		PinnedFunc: func(repo, key string) (bool, error) { return manager.IsPinned(context.Background(), repo, key) },
	})
	if err != nil {
		fmt.Fprintf(stderr, "export: %v\n", err)
		return exitFailure
	}
	if *flags.json {
		return writeJSONOutput(stdout, result, stderr)
	}
	fmt.Fprintf(stdout, "exported %d objects, %d bytes to %s\n", result.Objects, result.Bytes, result.To)
	fmt.Fprintf(stdout, "manifest: %s\n", filepath.Join(result.To, result.Manifest))
	return exitOK
}

func runImport(args []string, configPath string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("import", configPath, "", jsonDefault, stderr)
	from := flags.fs.String("from", "", "tree directory or tar archive")
	repo := flags.fs.String("repo", "", "only this repository")
	replace := flags.fs.Bool("replace", false, "replace existing objects")
	verify := flags.fs.Bool("verify", false, "verify imported blobs")
	dryRun := flags.fs.Bool("dry-run", false, "validate without writing")
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() != 0 || *from == "" {
		fmt.Fprintln(stderr, "import requires --from")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	st, idx, cleanup, err := openTransferTarget(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "open storage: %v\n", err)
		return exitFailure
	}
	defer cleanup()
	result, err := transfer.Import(context.Background(), st, idx, transfer.ImportOptions{
		From: *from, Repo: *repo, Replace: *replace, Verify: *verify, DryRun: *dryRun,
	})
	if err != nil {
		fmt.Fprintf(stderr, "import: %v\n", err)
		return exitFailure
	}
	if *flags.json {
		if code := writeJSONOutput(stdout, result, stderr); code != exitOK {
			return code
		}
	} else {
		fmt.Fprintf(stdout, "objects: %d\nblobs: %d\nskipped: %d\nfailed: %d\nbytes: %d\n",
			result.Objects, result.Blobs, result.Skipped, result.Failed, result.Bytes)
		if *verify {
			fmt.Fprintf(stdout, "verified: %d\n", result.Verified)
		}
	}
	if result.Failed > 0 {
		return exitFailure
	}
	return exitOK
}

func runIngest(args []string, configPath string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("ingest", configPath, "", jsonDefault, stderr)
	class := flags.fs.String("class", "immutable", "class to store")
	dryRun := flags.fs.Bool("dry-run", false, "classify without writing")
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: larder ingest <repo> <dir>")
		return exitUsage
	}
	if *class != "immutable" {
		fmt.Fprintln(stderr, "ingest stores immutable files only")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	repoName, dir := flags.fs.Arg(0), flags.fs.Arg(1)
	var repo config.Repo
	found := false
	for _, candidate := range cfg.Repos {
		if candidate.Name == repoName {
			repo = candidate
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintf(stderr, "unknown repo %q\n", repoName)
		return exitUsage
	}
	switch repo.Kind {
	case "go", "maven", "generic":
	default:
		fmt.Fprintf(stderr, "ingest supports go, maven and generic repositories, not %s\n", repo.Kind)
		return exitUsage
	}
	registry, err := transferRegistry()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailure
	}
	kind, err := registry.New(repo)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailure
	}
	st, idx, cleanup, err := openTransferTarget(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "open storage: %v\n", err)
		return exitFailure
	}
	defer cleanup()
	result, err := transfer.Ingest(context.Background(), st, idx, repo, kind, dir, transfer.IngestOptions{DryRun: *dryRun})
	if err != nil {
		fmt.Fprintf(stderr, "ingest: %v\n", err)
		return exitFailure
	}
	if *flags.json {
		if code := writeJSONOutput(stdout, result, stderr); code != exitOK {
			return code
		}
	} else {
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "stored\t%d\nbytes\t%d\nskipped\t%d\ndenied\t%d\nfailed\t%d\n",
			result.Stored, result.Bytes, result.Skipped, result.Denied, result.Failed)
		_ = tw.Flush()
		for _, message := range result.Errors {
			fmt.Fprintln(stdout, message)
		}
	}
	if result.Failed > 0 {
		return exitFailure
	}
	return exitOK
}

func openTransferTarget(cfg *config.Config) (*store.Store, *index.DB, func(), error) {
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		if !errors.Is(err, store.ErrLocked) {
			return nil, nil, nil, err
		}
		st, err = store.OpenReadOnly(cfg.DataDir)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	idx, err := index.Open(filepath.Join(cfg.DataDir, "index.db"))
	if err != nil {
		_ = st.Close()
		return nil, nil, nil, err
	}
	return st, idx, func() {
		_ = idx.Close()
		_ = st.Close()
	}, nil
}

func transferRegistry() (*kinds.Registry, error) {
	registry := kinds.NewRegistry()
	registrations := []struct {
		name string
		fn   func(config.Repo) (kinds.Kind, error)
	}{
		{"generic", generickind.New}, {"go", gokind.New}, {"maven", mavenkind.New},
		{"npm", npmkind.New}, {"pypi", pypikind.New}, {"crates", cratekind.New},
		{"rustup", rustupkind.New}, {"apt", aptkind.New}, {"alpine", alpinekind.New},
	}
	for _, registration := range registrations {
		if err := registry.Register(registration.name, registration.fn); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func removeFile(path string) error {
	return os.Remove(path)
}
