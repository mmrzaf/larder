package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/mmrzaf/larder/internal/admin"
	"github.com/mmrzaf/larder/internal/index"
	"github.com/mmrzaf/larder/internal/integrity"
	"github.com/mmrzaf/larder/internal/store"
)

func runVerify(args []string, configPath, adminURL string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("verify", configPath, adminURL, jsonDefault, stderr)
	repo := flags.fs.String("repo", "", "only this repository")
	sample := flags.fs.Int("sample", 100, "percentage of blobs to verify")
	rate := flags.fs.Int("rate", 50, "read rate in MB/s")
	repair := flags.fs.Bool("repair", false, "refetch corrupt objects through the admin API")
	checkExpectations := flags.fs.Bool("check-expectations", false, "compare objects with their expectations")
	dryRun := flags.fs.Bool("dry-run", false, "report corruption without removing it")
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() != 0 {
		fmt.Fprintln(stderr, "verify does not accept arguments")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	options := integrity.ScrubOptions{
		Repo: *repo, Sample: *sample, RateMBs: *rate,
		DryRun: *dryRun, CheckExpectations: *checkExpectations,
	}
	if client, _, err := adminClient(cfg, *flags.admin); err == nil && client != nil {
		body := map[string]any{
			"repo": *repo, "sample": *sample, "rate": *rate,
			"repair": *repair, "dry_run": *dryRun, "check_expectations": *checkExpectations,
		}
		var accepted struct {
			Job string `json:"job"`
		}
		err := client.Post(context.Background(), "/_larder/v1/verify", body, &accepted)
		if err == nil {
			var result integrity.ScrubResult
			if err := waitJob(context.Background(), client, accepted.Job, &result); err != nil {
				fmt.Fprintf(stderr, "verify: %v\n", err)
				return exitFailure
			}
			return printScrubResult(stdout, stderr, result, *flags.json)
		}
		if !errors.Is(err, admin.ErrUnreachable) {
			fmt.Fprintln(stderr, err)
			return exitFailure
		}
	}
	if *repair {
		fmt.Fprintln(stderr, "verify: repair requires a running server; corruption is removed without refetch")
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return directStorageError(err, stderr)
	}
	defer func() { _ = st.Close() }()
	idx, err := index.Open(filepath.Join(cfg.DataDir, "index.db"))
	if err != nil {
		fmt.Fprintf(stderr, "open index: %v\n", err)
		return exitFailure
	}
	defer func() { _ = idx.Close() }()
	scrubber, err := integrity.NewScrubber(st, idx, nil, nil, nil)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailure
	}
	result, err := scrubber.Scan(context.Background(), options)
	if err != nil {
		fmt.Fprintf(stderr, "verify: %v\n", err)
		return exitFailure
	}
	return printScrubResult(stdout, stderr, result, *flags.json)
}

func printScrubResult(stdout, stderr io.Writer, result integrity.ScrubResult, asJSON bool) int {
	if asJSON {
		if code := writeJSONOutput(stdout, result, stderr); code != exitOK {
			return code
		}
	} else {
		fmt.Fprintf(stdout, "checked: %d\n", result.Checked)
		fmt.Fprintf(stdout, "corrupt: %d\n", result.Corrupt)
		fmt.Fprintf(stdout, "quarantined: %d\n", result.Quarantined)
		fmt.Fprintf(stdout, "missing: %d\n", result.Missing)
		fmt.Fprintf(stdout, "removed objects: %d\n", result.RemovedObjects)
		if result.Repaired > 0 {
			fmt.Fprintf(stdout, "repaired: %d\n", result.Repaired)
		}
		fmt.Fprintf(stdout, "bytes read: %d\n", result.BytesRead)
		if len(result.CorruptBlobs) > 0 {
			tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SHA256\tREASON\tOBJECTS")
			for _, blob := range result.CorruptBlobs {
				fmt.Fprintf(tw, "%s\t%s\t%d\n", blob.SHA256, blob.Reason, blob.Objects)
			}
			_ = tw.Flush()
		}
		if len(result.Expectations) > 0 {
			fmt.Fprintln(stdout, "expectation mismatches:")
			for _, mismatch := range result.Expectations {
				fmt.Fprintf(stdout, "  %s/%s stored %s expected %s\n", mismatch.Repo, mismatch.Key,
					mismatch.SHA256, mismatch.Expected)
			}
		}
		fmt.Fprintf(stdout, "unverified: %d\n", result.Unverified)
	}
	if result.Corrupt > 0 {
		return exitFailure
	}
	return exitOK
}

func runReindex(args []string, configPath string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("reindex", configPath, "", jsonDefault, stderr)
	checkMode := flags.fs.Bool("check", false, "report disagreement")
	fix := flags.fs.Bool("fix", false, "repair what can be repaired")
	rebuild := flags.fs.Bool("rebuild-blobs-only", false, "create blob rows without keys")
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() != 0 {
		fmt.Fprintln(stderr, "reindex does not accept arguments")
		return exitUsage
	}
	modes := 0
	for _, enabled := range []bool{*checkMode, *fix, *rebuild} {
		if enabled {
			modes++
		}
	}
	if modes > 1 {
		fmt.Fprintln(stderr, "reindex accepts one of --check, --fix or --rebuild-blobs-only")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	if *rebuild {
		warningWriter := stdout
		if *flags.json {
			warningWriter = stderr
		}
		fmt.Fprintln(warningWriter, integrity.Warning(filepath.Join(cfg.DataDir, "index.db")))
	}
	if *fix || *rebuild {
		st, err := store.Open(cfg.DataDir)
		if err != nil {
			return directStorageError(err, stderr)
		}
		defer func() { _ = st.Close() }()
		idx, err := index.Open(filepath.Join(cfg.DataDir, "index.db"))
		if err != nil {
			fmt.Fprintf(stderr, "open index: %v\n", err)
			return exitFailure
		}
		defer func() { _ = idx.Close() }()
		if *rebuild {
			result, err := integrity.RebuildBlobsOnly(context.Background(), st, idx)
			if err != nil {
				fmt.Fprintf(stderr, "reindex: %v\n", err)
				return exitFailure
			}
			if *flags.json {
				return writeJSONOutput(stdout, result, stderr)
			}
			fmt.Fprintf(stdout, "blobs: %d\n", result.Blobs)
			for _, warning := range result.Warnings {
				fmt.Fprintln(stdout, warning)
			}
			return exitOK
		}
		report, err := integrity.Fix(context.Background(), st, idx, nil)
		if err != nil {
			fmt.Fprintf(stderr, "reindex: %v\n", err)
			return exitFailure
		}
		if *flags.json {
			if code := writeJSONOutput(stdout, report, stderr); code != exitOK {
				return code
			}
		} else {
			printReindexReport(stdout, report)
		}
		if report.Problems() > 0 {
			return 4
		}
		return exitOK
	}

	st, err := store.OpenReadOnly(cfg.DataDir)
	if err != nil {
		fmt.Fprintf(stderr, "open storage: %v\n", err)
		return exitFailure
	}
	defer func() { _ = st.Close() }()
	idx, err := index.OpenReadOnly(filepath.Join(cfg.DataDir, "index.db"))
	if err != nil {
		fmt.Fprintf(stderr, "open index: %v\n", err)
		return exitFailure
	}
	defer func() { _ = idx.Close() }()
	report, err := integrity.Reindex(context.Background(), st, idx)
	if err != nil {
		fmt.Fprintf(stderr, "reindex: %v\n", err)
		return exitFailure
	}
	if *flags.json {
		if code := writeJSONOutput(stdout, report, stderr); code != exitOK {
			return code
		}
	} else {
		printReindexReport(stdout, report)
	}
	if report.Problems() > 0 {
		return 4
	}
	return exitOK
}

func printReindexReport(stdout io.Writer, report integrity.ReindexReport) {
	fmt.Fprintf(stdout, "objects missing blobs: %d\n", len(report.ObjectsMissingBlobs))
	for _, object := range report.ObjectsMissingBlobs {
		fmt.Fprintf(stdout, "  %s/%s generation %d\n", object.Repo, object.Key, object.Generation)
	}
	fmt.Fprintf(stdout, "blobs without objects: %d\n", len(report.BlobsWithoutObjects))
	for _, blob := range report.BlobsWithoutObjects {
		fmt.Fprintf(stdout, "  %s %d bytes\n", blob.SHA256, blob.StoredSize)
	}
	fmt.Fprintf(stdout, "orphan files: %d\n", len(report.OrphanFiles))
	for _, file := range report.OrphanFiles {
		fmt.Fprintf(stdout, "  %s %d bytes\n", file.Path, file.Size)
	}
	fmt.Fprintf(stdout, "refcount mismatches: %d\n", len(report.RefcountMismatches))
	for _, mismatch := range report.RefcountMismatches {
		fmt.Fprintf(stdout, "  %s stored %d computed %d\n", mismatch.SHA256, mismatch.Stored, mismatch.Computed)
	}
	fmt.Fprintf(stdout, "duplicate current keys: %d\n", len(report.DuplicateCurrents))
	fmt.Fprintf(stdout, "expectation orphans: %d\n", report.ExpectationOrphans)
	fmt.Fprintf(stdout, "repair: %+v\n", report.Repair)
}

func runSnapshot(args []string, configPath string, jsonDefault bool, stdout, stderr io.Writer) int {
	flags := newStorageFlags("snapshot", configPath, "", jsonDefault, stderr)
	to := flags.fs.String("to", "", "snapshot destination")
	restore := flags.fs.String("restore", "", "snapshot to restore")
	if !flags.parse(args) {
		return exitUsage
	}
	if flags.fs.NArg() != 0 {
		fmt.Fprintln(stderr, "snapshot does not accept arguments")
		return exitUsage
	}
	if *to != "" && *restore != "" {
		fmt.Fprintln(stderr, "snapshot accepts --to or --restore, not both")
		return exitUsage
	}
	cfg, code := loadStorageConfig(*flags.config, stderr)
	if code != exitOK {
		return code
	}
	if *restore != "" {
		st, err := store.Open(cfg.DataDir)
		if err != nil {
			return directStorageError(err, stderr)
		}
		defer func() { _ = st.Close() }()
		if err := integrity.Restore(context.Background(), cfg.DataDir, *restore); err != nil {
			fmt.Fprintf(stderr, "restore: %v\n", err)
			return exitFailure
		}
		if *flags.json {
			return writeJSONOutput(stdout, map[string]any{"restored": *restore, "backup": filepath.Join(cfg.DataDir, "index.db.bak")}, stderr)
		}
		fmt.Fprintf(stdout, "restored %s (previous index kept as index.db.bak)\n", *restore)
		return exitOK
	}
	path := *to
	if path == "" {
		path = filepath.Join(cfg.DataDir, "snapshots", integrity.SnapshotName(time.Now()))
	}
	idx, err := index.OpenReadOnly(filepath.Join(cfg.DataDir, "index.db"))
	if err != nil {
		fmt.Fprintf(stderr, "open index: %v\n", err)
		return exitFailure
	}
	defer func() { _ = idx.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := integrity.SnapshotTo(ctx, idx, path); err != nil {
		fmt.Fprintf(stderr, "snapshot: %v\n", err)
		return exitFailure
	}
	if *flags.json {
		return writeJSONOutput(stdout, map[string]string{"snapshot": path}, stderr)
	}
	fmt.Fprintf(stdout, "snapshot: %s\n", path)
	return exitOK
}
