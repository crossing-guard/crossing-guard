package main

import (
	"fmt"
	"os"
	"os/exec"

	"crossing-guard/internal/daemon"
)

func console(args []string) {
	open, err := parseOpenArgs("console", args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "usage: crossing-guard console [--open]")
		os.Exit(2)
	}
	location, err := daemon.LocateConsole()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if problem := daemon.ProbeConsole(location); problem != "" {
		fmt.Fprintln(os.Stderr, problem)
		os.Exit(1)
	}
	fmt.Println(location.URL())
	fmt.Println("  data:   " + location.DataDir)
	fmt.Println("  found:  " + location.Source)
	if open {
		if err := exec.Command("open", location.URL()).Run(); err != nil {
			fmt.Fprintln(os.Stderr, "could not launch a browser ("+err.Error()+") — open the URL above yourself")
		}
	}
}
