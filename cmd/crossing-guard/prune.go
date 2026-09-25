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
func prune(args []string) {
	keepDays, before, confirm := 0, "", false
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
			confirm = true
		}
	}
	var cutoff int64
	switch {
	case before != "":
		parsed, err := time.Parse("2006-01-02", before)
		if err != nil {
			fmt.Fprintf(os.Stderr, "--before must be YYYY-MM-DD: %v\n", err)
			os.Exit(2)
		}
		cutoff = parsed.Unix()
	case keepDays > 0:
		cutoff = time.Now().AddDate(0, 0, -keepDays).Unix()
	default:
		fmt.Fprintln(os.Stderr, "prune needs an explicit horizon: --keep-days N or --before YYYY-MM-DD")
		fmt.Fprintln(os.Stderr, "(nothing is deleted without one — this trims PRIMARY TRUTH)")
		os.Exit(2)
	}
	index, err := store.Open(store.IndexPath("", mustHome()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		os.Exit(1)
	}
	count, err := index.CountEventsBefore(cutoff)
	if err != nil {
		closeErr := index.Close()
		fmt.Fprintln(os.Stderr, "count:", err)
		if closeErr != nil {
			fmt.Fprintln(os.Stderr, "close store:", closeErr)
		}
		os.Exit(1)
	}
	horizon := time.Unix(cutoff, 0).Format("2006-01-02 15:04")
	if count == 0 || !confirm {
		if err := index.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "close store:", err)
			os.Exit(1)
		}
		if count == 0 {
			fmt.Printf("no events older than %s — nothing to prune\n", horizon)
			return
		}
		fmt.Printf("DRY RUN: %d events older than %s would be deleted.\n", count, horizon)
		fmt.Println("This deletes PRIMARY TRUTH (the only record that a blocked action happened).")
		fmt.Println("Export first:  crossing-guard export")
		fmt.Println("Then re-run with --yes to actually delete.")
		return
	}
	deleted, err := index.PruneEventsBefore(cutoff)
	if err != nil {
		closeErr := index.Close()
		fmt.Fprintln(os.Stderr, "prune:", err)
		if closeErr != nil {
			fmt.Fprintln(os.Stderr, "close store:", closeErr)
		}
		os.Exit(1)
	}
	if err := index.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "close store:", err)
		os.Exit(1)
	}
	fmt.Printf("pruned %d events older than %s\n", deleted, horizon)
	fmt.Println("disk is reclaimed on the next `crossing-guard export` or a VACUUM, not immediately")
}
