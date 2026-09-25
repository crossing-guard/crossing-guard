package main

import (
	"fmt"
	"os"

	"crossing-guard/internal/daemon"
	"crossing-guard/internal/guardcli"
)

// uninstall reverses hooks and service while preserving the local record by default;
// irreversible data removal requires the separate, explicit --purge choice.
func uninstall(args []string) {
	purge := false
	for _, argument := range args {
		if argument == "--purge" {
			purge = true
		}
	}
	fmt.Println("crossing-guard uninstall")
	fmt.Println("\nhooks:")
	for _, status := range guardcli.UninstallHooks() {
		fmt.Printf("  %-7s %-8s %s\n", status.Vendor, status.Action, status.Detail)
	}
	fmt.Println("\nservice:")
	service := daemon.RemoveService()
	fmt.Printf("  %-8s %s\n", service.Action, service.Detail)
	fmt.Println("\ndata:")
	directory := dataDir()
	if !purge {
		fmt.Printf("  kept     %s\n", directory)
		fmt.Println("  (append-only governance record left in place; re-run with --purge to delete it)")
		return
	}
	if _, err := os.Stat(directory); os.IsNotExist(err) {
		fmt.Printf("  absent   %s — nothing to purge\n", directory)
		return
	}
	if err := os.RemoveAll(directory); err != nil {
		fmt.Printf("  error    could not purge %s: %v\n", directory, err)
		os.Exit(1)
	}
	fmt.Printf("  purged   %s (governance record deleted — this is irreversible)\n", directory)
}
