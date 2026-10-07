package memcli

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/internal/transcriptindex"
)

// cmdSessions dispatches `sessions <verb>` to a per-verb helper.
func cmdSessions(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		sessionsList(args[1:])
	case "show":
		sessionsShow(args[1:])
	case "search":
		sessionsSearch(args[1:])
	default:
		usage()
		os.Exit(2)
	}
}

// printSessionRow is the ONE session-list row format, used by both the index
// fast path and the scan fallback so they cannot drift (they had a duplicated
// Printf before).
// The count is model calls (token-usage-analytics plan §20): the summary's
// Turns counts calls for every runtime.
func printSessionRow(vendor, id, when, project string, calls int, title string) {
	fmt.Printf("%-6s %-10s %s  %-28s %3d calls  %s\n",
		vendor, shortID(id), when, truncate(project, 28), calls, title)
}

// printSessionHit is the one indexed search-hit row format. Title documents are
// deliberately visible as titles rather than looking like transcript statements.
func printSessionHit(vendor, sessionID, ts, kind, text string) {
	if kind == "title" {
		text = "[title] " + text
	}
	fmt.Printf("%-6s %-10s %s %-9s %s\n",
		vendor, shortID(sessionID), ts, kind, truncate(strings.ReplaceAll(text, "\n", " "), 120))
}

func transcriptCoverageWarning(coverage transcriptindex.Coverage) string {
	if coverage.State == transcriptindex.CoverageCurrent {
		return ""
	}
	return fmt.Sprintf("Transcript search coverage is %s; results may be incomplete. Run `crossing-guard harvest` to refresh.",
		coverage.State)
}

func sessionsList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	vendor := fs.String("vendor", "", strings.Join(harvest.RuntimeNames(), "|"))
	limit := fs.Int("limit", 25, "")
	project := fs.String("project", "", "substring filter on cwd")
	asJSON := fs.Bool("json", false, "")
	fs.Parse(args)
	if indexExists() { // fast path (ADR 0013 D1); falls back to scan below
		rows, err := indexListSessions(*vendor, *project, *limit)
		if err == nil {
			if *asJSON {
				printJSON(rows)
				return
			}
			for _, r := range rows {
				printSessionRow(r.Vendor, r.ID, time.Unix(r.Modified, 0).Format("2006-01-02 15:04"),
					r.Project, r.Turns, r.Title)
			}
			return
		}
		fmt.Fprintln(os.Stderr, "index error (falling back to scan):", err)
	}
	var rows []Session
	for _, s := range ListSessions(*vendor) {
		if *project != "" && !strings.Contains(s.CWD, *project) {
			continue
		}
		rows = append(rows, s)
		if len(rows) >= *limit {
			break
		}
	}
	if *asJSON {
		printJSON(rows)
		return
	}
	for _, s := range rows {
		printSessionRow(s.Vendor, s.ID, s.Modified.Format("2006-01-02 15:04"),
			transcriptindex.ProjectLabel(s.CWD), s.Turns, s.Title)
	}
}

func sessionsShow(args []string) {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	limit := fs.Int("limit", 200, "max events")
	pos, flagArgs := splitPositional(args)
	fs.Parse(flagArgs)
	if len(pos) < 1 {
		usage()
		os.Exit(2)
	}
	s, ok := findSession(pos[0])
	if !ok {
		fmt.Fprintln(os.Stderr, "no session matches", pos[0])
		os.Exit(1)
	}
	fmt.Printf("# %s %s  cwd=%s  %s\n\n", s.Vendor, s.ID, s.CWD, s.Title)
	for i, ev := range SessionEvents(s) {
		if i >= *limit {
			fmt.Println("…")
			break
		}
		fmt.Printf("%-11s | %s\n", ev.Kind, truncate(strings.ReplaceAll(ev.Text, "\n", " "), 160))
	}
}

func sessionsSearch(args []string) {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	limit := fs.Int("limit", 20, "")
	asJSON := fs.Bool("json", false, "")
	pos, flagArgs := splitPositional(args)
	fs.Parse(flagArgs)
	if len(pos) < 1 {
		usage()
		os.Exit(2)
	}
	query := strings.Join(pos, " ")
	result, _ := indexSearchEvents(query, *limit)
	if *asJSON {
		printJSON(result)
		return
	}
	if warning := transcriptCoverageWarning(result.Coverage); warning != "" {
		fmt.Fprintln(os.Stderr, warning)
	}
	for _, hit := range result.Hits {
		printSessionHit(hit.Vendor, hit.SessionID, shortDate(hit.TS), hit.Kind, hit.Text)
	}
}
