package main

// bundle (team rest-of-release plan §4.3): `bundle build` asks the local daemon on a
// linked device to build the next revision of a scope's bundle, unsigned, and prints
// what changed; `bundle sign` is a plain command — no daemon — that signs that file
// with a key only the lead holds and writes the signed document beside it.

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"crossing-guard/internal/daemon"
	"crossing-guard/internal/repofile"
	"crossing-guard/teamwire"
)

const bundleUsage = `usage: crossing-guard bundle build --scope organization|repository [--agent <id>]... [--remove-agent <id>]...
                                   [--rules] [--failure-mode fail-open|fail-closed]
                                   [--content-policy off|consent|mandated] [--expires-in <duration>] [--out <file>]
       crossing-guard bundle sign --key <path> <file>`

// bundleBuildRequest is the daemon's build request as this verb sends it.
type bundleBuildRequest struct {
	Scope         string   `json:"scope"`
	Cwd           string   `json:"cwd,omitempty"`
	Agents        []string `json:"agents,omitempty"`
	RemoveAgents  []string `json:"remove_agents,omitempty"`
	Rules         bool     `json:"rules,omitempty"`
	FailureMode   string   `json:"failure_mode,omitempty"`
	ContentPolicy string   `json:"content_policy,omitempty"`
	ExpiresIn     string   `json:"expires_in,omitempty"`
}

// bundleBuildResult is the part of the daemon's answer this verb prints.
type bundleBuildResult struct {
	Path              string `json:"path"`
	SignedPath        string `json:"signed_path"`
	SignCommand       string `json:"sign_command"`
	BundleID          string `json:"bundle_id"`
	Scope             string `json:"scope"`
	Revision          int64  `json:"revision"`
	PublishedRevision int64  `json:"published_revision"`
	FailureMode       string `json:"failure_mode"`
	ExpiresAt         string `json:"expires_at"`
	Documents         []struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Change    string `json:"change"`
		ProfileID string `json:"profile_id"`
		TextDiff  []struct {
			Op   string `json:"op"`
			Text string `json:"text"`
		} `json:"text_diff"`
	} `json:"documents"`
	RuleChanges *struct {
		Added   []string `json:"added"`
		Removed []string `json:"removed"`
		Changed []struct {
			ID string `json:"id"`
		} `json:"changed"`
	} `json:"rule_changes"`
	Changes []struct {
		Label string `json:"label"`
		From  string `json:"from"`
		To    string `json:"to"`
	} `json:"changes"`
}

// bundleBuilder asks a daemon to build; the verb's tests supply their own.
type bundleBuilder func(bundleBuildRequest) (bundleBuildResult, error)

func bundleCmd(args []string) {
	os.Exit(runBundle(args, os.Stdout, os.Stderr, func(req bundleBuildRequest) (bundleBuildResult, error) {
		var out bundleBuildResult
		loc, err := daemon.LocateConsole()
		if err != nil {
			return out, fmt.Errorf("no local daemon to ask: %w", err)
		}
		if err := loc.PostJSON("/api/team/bundles/build", req, &out); err != nil {
			return out, errors.New(linkError(err))
		}
		return out, nil
	}))
}

func runBundle(args []string, stdout, stderr io.Writer, build bundleBuilder) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, bundleUsage)
		return 2
	}
	switch args[0] {
	case "build":
		return bundleBuild(args[1:], stdout, stderr, build)
	case "sign":
		return bundleSign(args[1:], stdout, stderr)
	}
	fmt.Fprintln(stderr, bundleUsage)
	return 2
}

// parseBundleBuild reads the build flags. Every flag that takes a value takes it as
// the next argument.
func parseBundleBuild(args []string) (bundleBuildRequest, string, error) {
	var req bundleBuildRequest
	out := ""
	for index := 0; index < len(args); index++ {
		flag := args[index]
		if flag == "--rules" {
			req.Rules = true
			continue
		}
		if index+1 >= len(args) {
			return req, "", fmt.Errorf("%s needs a value", flag)
		}
		value := args[index+1]
		index++
		switch flag {
		case "--scope":
			req.Scope = value
		case "--agent":
			req.Agents = append(req.Agents, value)
		case "--remove-agent":
			req.RemoveAgents = append(req.RemoveAgents, value)
		case "--failure-mode":
			req.FailureMode = value
		case "--content-policy":
			req.ContentPolicy = value
		case "--expires-in":
			req.ExpiresIn = value
		case "--out":
			out = value
		default:
			return req, "", fmt.Errorf("unknown option %s", flag)
		}
	}
	if req.Scope == "" {
		return req, "", errors.New("--scope is required")
	}
	return req, out, nil
}

func bundleBuild(args []string, stdout, stderr io.Writer, build bundleBuilder) int {
	req, out, err := parseBundleBuild(args)
	if err != nil {
		fmt.Fprintln(stderr, "bundle build:", err)
		fmt.Fprintln(stderr, bundleUsage)
		return 2
	}
	if cwd, err := os.Getwd(); err == nil {
		req.Cwd = cwd
	}
	// Refused before anything is built: a path that will not be written builds nothing.
	if err := bundleOutAllowed(out); err != nil {
		fmt.Fprintln(stderr, "bundle build:", err)
		return 1
	}
	result, err := build(req)
	if err != nil {
		fmt.Fprintln(stderr, "bundle build refused:", err)
		return 1
	}
	path := result.Path
	if out != "" {
		// The daemon writes under its data directory; --out is this command's own copy
		// to the place the lead named.
		if err := copyNewFile(result.Path, out); err != nil {
			fmt.Fprintln(stderr, "bundle build:", err)
			return 1
		}
		path = out
		result.SignedPath = teamwire.SignedBundlePath(out)
		result.SignCommand = strings.Replace(result.SignCommand, result.Path, out, 1)
	}
	printBundleBuild(stdout, result, path)
	return 0
}

// bundleOutAllowed refuses an --out path inside a git checkout, as `org-key init`
// refuses one for a key: the unsigned bundle is the team's rules and agents on their
// way to being signed, and a copy in a checkout can be committed or changed by an
// agent working there before it is.
func bundleOutAllowed(out string) error {
	if out == "" {
		return nil
	}
	absolute, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	folder, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return fmt.Errorf("the folder %s does not exist", filepath.Dir(absolute))
	}
	if root, inCheckout := repofile.Locate(folder); inCheckout {
		return fmt.Errorf("%s is inside the git checkout %s; an unsigned bundle there can be committed or changed by an agent working in it — choose a place outside any checkout",
			filepath.Join(folder, filepath.Base(absolute)), root)
	}
	return nil
}

func printBundleBuild(stdout io.Writer, result bundleBuildResult, path string) {
	fmt.Fprintf(stdout, "built %s revision %d (bundle %s), unsigned\n", result.Scope, result.Revision, result.BundleID)
	if result.PublishedRevision > 0 {
		fmt.Fprintf(stdout, "against the published revision %d:\n", result.PublishedRevision)
	} else {
		fmt.Fprintln(stdout, "this scope has no published bundle yet:")
	}
	for _, change := range result.Changes {
		fmt.Fprintf(stdout, "  %s: %s → %s\n", change.Label, orNone(change.From), orNone(change.To))
	}
	for _, document := range result.Documents {
		label := document.Name
		if document.ProfileID != "" {
			label = "agent " + document.ProfileID
		}
		fmt.Fprintf(stdout, "  %s %s (%s)\n", document.Change, label, document.Kind)
		for _, line := range document.TextDiff {
			switch line.Op {
			case "add":
				fmt.Fprintln(stdout, "    + "+line.Text)
			case "remove":
				fmt.Fprintln(stdout, "    - "+line.Text)
			}
		}
	}
	if result.RuleChanges != nil {
		for _, id := range result.RuleChanges.Added {
			fmt.Fprintln(stdout, "  rule added: "+id)
		}
		for _, id := range result.RuleChanges.Removed {
			fmt.Fprintln(stdout, "  rule removed: "+id)
		}
		for _, rule := range result.RuleChanges.Changed {
			fmt.Fprintln(stdout, "  rule changed: "+rule.ID)
		}
	}
	fmt.Fprintf(stdout, "failure mode %s, expires %s\n", result.FailureMode, result.ExpiresAt)
	fmt.Fprintln(stdout, "file:   "+path)
	fmt.Fprintln(stdout, "sign:   "+result.SignCommand)
	fmt.Fprintln(stdout, "upload: "+result.SignedPath+" in the team console → Policy → Upload signed bundle")
}

func orNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

// copyNewFile copies from to a file that must not exist yet, private to the user.
func copyNewFile(from, to string) error {
	raw, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// bundleSign signs an unsigned bundle file with the key file the lead names and
// writes "…-r<revision>.signed.json" beside the input. It signs the canonical bytes
// a device verifies, with the key id derived from the key.
func bundleSign(args []string, stdout, stderr io.Writer) int {
	if len(args) != 3 || args[0] != "--key" {
		fmt.Fprintln(stderr, bundleUsage)
		return 2
	}
	keyPath, bundlePath := args[1], args[2]
	private, err := readOrgKey(keyPath)
	if err != nil {
		fmt.Fprintln(stderr, "bundle sign:", err)
		return 1
	}
	unsigned, err := os.ReadFile(bundlePath)
	if err != nil {
		fmt.Fprintln(stderr, "bundle sign:", err)
		return 1
	}
	var facts teamwire.SignedBundle
	if err := json.Unmarshal(unsigned, &facts); err != nil || facts.ID == "" || facts.Revision < 1 {
		fmt.Fprintf(stderr, "bundle sign: %s is not a bundle built by crossing-guard bundle build\n", bundlePath)
		return 1
	}
	public := private.Public().(ed25519.PublicKey)
	signed, err := teamwire.SignBundle(unsigned, private, teamwire.OrgKeyID(public))
	if err != nil {
		fmt.Fprintln(stderr, "bundle sign:", err)
		return 1
	}
	out := teamwire.SignedBundlePath(bundlePath)
	if err := os.WriteFile(out, append(signed, '\n'), 0o600); err != nil {
		fmt.Fprintln(stderr, "bundle sign:", err)
		return 1
	}
	fmt.Fprintf(stdout, "signed bundle %s revision %d with key %s (%s)\n", facts.ID, facts.Revision,
		teamwire.OrgKeyID(public), teamwire.KeyFingerprint(public))
	fmt.Fprintln(stdout, "wrote "+out)
	fmt.Fprintln(stdout, "Upload it in the team console: Policy → Upload signed bundle.")
	return 0
}
