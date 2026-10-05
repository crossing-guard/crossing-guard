package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"crossing-guard/store"
)

// prune trims PRIMARY TRUTH only behind an explicit horizon and dry-runs unless the
// user supplies --yes. It never becomes an automatic background retention policy.
// (Derived understanding facts are not primary truth and are not trimmed here: the
// daemon's scan coordinator retains them, understanding-facts-retention plan.)
// Without --usage it trims events; with --usage it trims recorded usage calls
// instead, which exist to outlive vendor pruning (token-usage-analytics plan D-12).
func prune(args []string) {
	request := parsePruneArgs(args)
	index, err := store.Open(store.IndexPath("", mustHome()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		os.Exit(1)
	}
	var runErr error
	if request.usage {
		runErr = pruneUsage(index, request)
	} else {
		runErr = pruneEvents(index, request)
	}
	closeErr := index.Close()
	if runErr != nil {
		fmt.Fprintln(os.Stderr, "prune:", runErr)
	}
	if closeErr != nil {
		fmt.Fprintln(os.Stderr, "close store:", closeErr)
	}
	if runErr != nil || closeErr != nil {
		os.Exit(1)
	}
}

// pruneRequest is one prune invocation: a horizon in unix seconds, whether to
// delete, and which record it trims.
type pruneRequest struct {
	cutoff  int64
	confirm bool
	usage   bool
}

func parsePruneArgs(args []string) pruneRequest {
	keepDays, before, request := 0, "", pruneRequest{}
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--keep-days":
			if index+1 < len(args) {
				value, err := strconv.Atoi(args[index+1])
				if err != nil {
					fmt.Fprintf(os.Stderr, "--keep-days must be an integer: %q\n", args[index+1])
					os.Exit(2)
				}
				keepDays, index = value, index+1
			}
		case "--before":
			if index+1 < len(args) {
				before, index = args[index+1], index+1
			}
		case "--yes":
			request.confirm = true
		case "--usage":
			request.usage = true
		}
	}
	switch {
	case before != "":
		parsed, err := time.Parse("2006-01-02", before)
		if err != nil {
			fmt.Fprintf(os.Stderr, "--before must be YYYY-MM-DD: %v\n", err)
			os.Exit(2)
		}
		request.cutoff = parsed.Unix()
	case keepDays > 0:
		request.cutoff = time.Now().AddDate(0, 0, -keepDays).Unix()
	default:
		fmt.Fprintln(os.Stderr, "prune needs an explicit horizon: --keep-days N or --before YYYY-MM-DD")
		fmt.Fprintln(os.Stderr, "(nothing is deleted without one — this trims PRIMARY TRUTH)")
		os.Exit(2)
	}
	return request
}

func pruneEvents(index *store.Index, request pruneRequest) error {
	count, err := index.CountEventsBefore(request.cutoff)
	if err != nil {
		return fmt.Errorf("count: %w", err)
	}
	horizon := time.Unix(request.cutoff, 0).Format("2006-01-02 15:04")
	if count == 0 {
		fmt.Printf("no events older than %s — nothing to prune\n", horizon)
		return nil
	}
	if !request.confirm {
		fmt.Printf("DRY RUN: %d events older than %s would be deleted.\n", count, horizon)
		fmt.Println("This deletes PRIMARY TRUTH (the only record that a blocked action happened).")
		fmt.Println("Export first:  crossing-guard export")
		fmt.Println("Then re-run with --yes to actually delete.")
		return nil
	}
	deleted, err := index.PruneEventsBefore(request.cutoff)
	if err != nil {
		return err
	}
	fmt.Printf("pruned %d events older than %s\n", deleted, horizon)
	fmt.Println("the store file shrinks on the next `crossing-guard compact` (daemon stopped), not immediately")
	return nil
}

// pruneUsage trims recorded usage calls. Calls are stamped in milliseconds; the
// horizon becomes permanent, so no later re-read of a vendor source brings the
// pruned calls back.
func pruneUsage(index *store.Index, request pruneRequest) error {
	cutoffMS := time.Unix(request.cutoff, 0).UnixMilli()
	count, err := index.CountUsageCallsBefore(cutoffMS)
	if err != nil {
		return fmt.Errorf("count: %w", err)
	}
	horizon := time.Unix(request.cutoff, 0).Format("2006-01-02 15:04")
	// The horizon is recorded even when nothing older is recorded yet: the
	// backfill reads newest first, and a rollback empties the tables, so older
	// calls may still arrive (code red-team C-4).
	if !request.confirm {
		fmt.Printf("DRY RUN: %d usage calls older than %s would be deleted,\n", count, horizon)
		fmt.Println("and no call older than that would be recorded again, even from vendor files still on disk.")
		fmt.Println("Export first:  crossing-guard export")
		fmt.Println("Then re-run with --yes to actually delete.")
		return nil
	}
	deleted, err := index.PruneUsageCallsBefore(cutoffMS)
	if err != nil {
		return err
	}
	fmt.Printf("pruned %d usage calls older than %s; none older will be recorded again\n", deleted, horizon)
	fmt.Println("the store file shrinks on the next `crossing-guard compact` (daemon stopped), not immediately")
	return nil
}
