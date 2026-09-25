package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"crossing-guard/internal/analyzerhost"
	"crossing-guard/internal/changeenv"
	"crossing-guard/store"
)

type stringsFlag []string

func (s *stringsFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringsFlag) Set(v string) error { *s = append(*s, v); return nil }

type commonChange struct{ session, repo, runtime, title string }

func commonFlags(fs *flag.FlagSet, c *commonChange) {
	fs.StringVar(&c.session, "session", "", "session id")
	fs.StringVar(&c.repo, "repo", "", "repository directory")
	fs.StringVar(&c.runtime, "runtime", "", "claimed runtime metadata")
	fs.StringVar(&c.title, "title", "", "claimed title metadata")
}
func parseDataSubcommand(command string, args []string) (string, string, []string, error) {
	data := ""
	if len(args) > 0 && args[0] == "--data" {
		if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
			return "", "", nil, fmt.Errorf("--data requires a directory")
		}
		data = args[1]
		args = args[2:]
	} else if len(args) > 0 && strings.HasPrefix(args[0], "--data=") {
		data = strings.TrimPrefix(args[0], "--data=")
		if strings.TrimSpace(data) == "" {
			return "", "", nil, fmt.Errorf("--data requires a directory")
		}
		args = args[1:]
	}
	if len(args) == 0 {
		return "", "", nil, fmt.Errorf("missing %s subcommand", command)
	}
	return data, args[0], args[1:], nil
}

func runChange(args []string) int {
	data, sub, rest, err := parseDataSubcommand("change", args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	switch sub {
	case "declare", "snapshot", "claim", "verify-claim", "verify-run", "show":
	default:
		fmt.Fprintln(os.Stderr, "unknown change subcommand:", sub)
		return 2
	}
	path := store.IndexPath(data, mustHome())
	var ix *store.Index
	if sub == "show" {
		ix, err = store.OpenRO(path)
	} else {
		ix, err = store.Open(path)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		return 1
	}
	defer func() { _ = ix.Close() }()
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, sub+":", err)
		fmt.Fprintln(os.Stderr, "store:", path)
		return 1
	}
	switch sub {
	case "declare", "claim":
		fs := flag.NewFlagSet(sub, flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		var c commonChange
		commonFlags(fs, &c)
		source, intent := "", ""
		var paths, symbols stringsFlag
		fs.StringVar(&source, "source-file", "", "required claim source file")
		fs.StringVar(&intent, "intent", "", "claimed intent label")
		fs.Var(&paths, "path", "repository-relative path (repeatable)")
		fs.Var(&symbols, "symbol", "PATH::SYMBOL (repeatable)")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		kind := "implementation"
		if sub == "declare" {
			kind = "declaration"
		}
		r, err := changeenv.RecordClaim(ix, kind, changeenv.ClaimInput{SessionID: c.session, Runtime: c.runtime, Title: c.title, RepoDir: c.repo, SourcePath: source, Intent: intent, Paths: paths, Symbols: symbols})
		if err != nil {
			return fail(err)
		}
		printChangeRecord(path, r)
		return 0
	case "snapshot":
		fs := flag.NewFlagSet(sub, flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		var c commonChange
		commonFlags(fs, &c)
		base := ""
		fs.StringVar(&base, "base", "", "required base ref")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		r, err := changeenv.RecordSnapshot(ix, changeenv.SnapshotInput{SessionID: c.session, Runtime: c.runtime, Title: c.title, RepoDir: c.repo, Base: base})
		if err != nil {
			return fail(err)
		}
		printChangeRecord(path, r)
		return 0
	case "verify-claim":
		fs := flag.NewFlagSet(sub, flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		var c commonChange
		commonFlags(fs, &c)
		source, name, boundary, result := "", "", "", ""
		fs.StringVar(&source, "source-file", "", "required claim source file")
		fs.StringVar(&name, "name", "", "claimed check name")
		fs.StringVar(&boundary, "boundary", "", "claimed boundary")
		fs.StringVar(&result, "result", "", "pass|fail|unavailable")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		r, err := changeenv.RecordVerificationClaim(ix, changeenv.VerificationClaimInput{SessionID: c.session, Runtime: c.runtime, Title: c.title, RepoDir: c.repo, SourcePath: source, Name: name, Boundary: boundary, Result: result})
		if err != nil {
			return fail(err)
		}
		printChangeRecord(path, r)
		return 0
	case "verify-run":
		fs := flag.NewFlagSet(sub, flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		var c commonChange
		commonFlags(fs, &c)
		cwd, name, boundary := "", "", ""
		timeout := time.Duration(0)
		fs.StringVar(&cwd, "cwd", "", "command cwd")
		fs.StringVar(&name, "name", "", "claimed check name")
		fs.StringVar(&boundary, "boundary", "", "claimed boundary")
		fs.DurationVar(&timeout, "timeout", 0, "required timeout")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		command := fs.Args()
		if len(command) > 0 && command[0] == "--" {
			command = command[1:]
		}
		r, code, err := changeenv.VerifyRun(ix, changeenv.VerifyInput{SessionID: c.session, Runtime: c.runtime, Title: c.title, RepoDir: c.repo, CWD: cwd, Name: name, Boundary: boundary, Timeout: timeout, Command: command})
		if err != nil {
			return fail(err)
		}
		printChangeRecord(path, r)
		return code
	case "show":
		fs := flag.NewFlagSet(sub, flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		session := ""
		asJSON := false
		var opt changeenv.ViewOptions
		fs.StringVar(&session, "session", "", "session id")
		fs.BoolVar(&asJSON, "json", false, "JSON output")
		fs.IntVar(&opt.RepositoryOffset, "repository-offset", 0, "repository page offset")
		fs.IntVar(&opt.RepositoryLimit, "repository-limit", 0, "repository page size (max 1)")
		fs.IntVar(&opt.FileOffset, "change-offset", 0, "file row page offset")
		fs.IntVar(&opt.FileLimit, "change-limit", 0, "file row page size (max 200)")
		fs.StringVar(&opt.FileFilter, "change-filter", "", "all|session|outside-plan|planned|touched|claimed|changed")
		fs.StringVar(&opt.FileQuery, "change-query", "", "case-insensitive repository-relative path substring")
		fs.StringVar(&opt.FileSort, "change-sort", "", "path|overlap")
		fs.IntVar(&opt.ObservedOffset, "observed-offset", 0, "observed target page offset")
		fs.IntVar(&opt.ObservedLimit, "observed-limit", 0, "observed target page size (max 200)")
		fs.StringVar(&opt.ObservedScope, "observed-scope", "", "observed target relationship filter")
		var sessionRoots stringsFlag
		fs.Var(&sessionRoots, "session-root", "recorded session working directory (repeatable)")
		fs.IntVar(&opt.AddedOffset, "added-offset", 0, "added-scope page offset")
		fs.IntVar(&opt.OmittedOffset, "omitted-offset", 0, "omitted-scope page offset")
		fs.IntVar(&opt.VerificationOffset, "verification-offset", 0, "verification page offset")
		center := ""
		fs.StringVar(&center, "impact-center", "", "optional KIND:REF impact center")
		fs.IntVar(&opt.ImpactNodeOffset, "impact-node-offset", 0, "impact node page offset")
		fs.IntVar(&opt.ImpactEdgeOffset, "impact-edge-offset", 0, "impact edge page offset")
		fs.IntVar(&opt.ImpactLimit, "impact-limit", 0, "impact page size (max 100)")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if strings.TrimSpace(session) == "" {
			return fail(fmt.Errorf("show requires session"))
		}
		opt.SessionRoots = sessionRoots
		if center != "" {
			var ok bool
			opt.ImpactCenterKind, opt.ImpactCenterRef, ok = strings.Cut(center, ":")
			if !ok || opt.ImpactCenterKind == "" || opt.ImpactCenterRef == "" {
				return fail(fmt.Errorf("impact-center must be KIND:REF"))
			}
		}
		host, hostErr := analyzerhost.Build(effectiveAnalyzerDataDir(data))
		if hostErr != nil {
			return fail(hostErr)
		}
		opt.AnalyzerBundleDigest = host.Assembly.Digest()
		v, err := changeenv.Build(ix, session, opt)
		if err != nil {
			return fail(err)
		}
		if asJSON {
			b, _ := json.MarshalIndent(struct {
				Store  string                `json:"store"`
				Change changeenv.SessionView `json:"change"`
			}{path, v}, "", "  ")
			fmt.Println(string(b))
		} else {
			fmt.Printf("store: %s\nsession: %s\nchange: %s\n", path, session, changeenv.Summary(v))
		}
		return 0
	}
	return 2
}
func printChangeRecord(path string, r *store.ChangeRecord) {
	fmt.Printf("recorded %s #%d\nstore: %s\nrepository: %s\ncheckout: %s\n", r.Kind, r.ID, path, r.RepositoryID, r.CheckoutID)
}
