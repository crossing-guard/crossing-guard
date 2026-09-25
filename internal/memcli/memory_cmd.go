package memcli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// cmdMemory dispatches `memory <verb>` to a per-verb helper. Each verb is its
// own nameable job; this stays a thin switch that reads at one altitude.
func cmdMemory(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	dir := memoryDir()
	rest := args[1:]
	switch args[0] {
	case "upsert":
		memUpsert(dir, rest)
	case "get":
		memGet(dir, rest)
	case "list":
		memList(dir, rest)
	case "promote":
		memPromote(dir, rest)
	case "reject":
		memReject(dir, rest)
	case "delete":
		memDelete(dir, rest)
	case "search":
		memSearch(dir, rest)
	case "index":
		memIndex(dir, rest)
	case "log":
		memLog(dir, rest)
	case "edit":
		memEdit(dir, rest)
	case "new":
		memNew(dir, rest)
	case "note":
		memNote(dir, rest)
	case "verify":
		memVerify(dir, rest)
	default:
		usage()
		os.Exit(2)
	}
}

// categoriesHelp derives the --category help from memory.Categories so the enum
// is declared once (an earlier hardcoded copy had already drifted — it omitted
// "note", a valid category).
func categoriesHelp() string {
	ks := make([]string, 0, len(categories))
	for k := range categories {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, "|")
}

func memUpsert(dir string, args []string) {
	fs := flag.NewFlagSet("upsert", flag.ExitOnError)
	id := fs.String("id", "", "kebab-case slug (reuse = update in place)")
	title := fs.String("title", "", "")
	category := fs.String("category", "", categoriesHelp())
	tags := fs.String("tags", "", "comma-separated")
	aliases := fs.String("aliases", "", "comma-separated identifier spellings (ch8, channel-8, …)")
	repo := fs.String("repository", "", "")
	source := fs.String("source", "human", "human|agent|harvest")
	content := fs.String("content", "", "body (else read from stdin)")
	fs.Parse(args)
	if *id == "" || *title == "" || *category == "" {
		fmt.Fprintln(os.Stderr, "--id, --title, --category required")
		os.Exit(2)
	}
	body := *content
	if body == "" {
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			// the body is a domain value — a truncated read must not silently
			// write a partial record that then passes validation and commits
			fmt.Fprintln(os.Stderr, "read stdin:", err)
			os.Exit(1)
		}
		body = string(raw)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	rec := Record{ID: *id, Title: *title, Category: *category,
		Repository: *repo, Tags: splitCSV(*tags), Aliases: splitCSV(*aliases),
		Source: *source, Created: now, Updated: now, Body: body}
	updating := false
	if old, err := readRecord(dirJoin(dir, *id+".md")); err == nil {
		rec.Created = old.Created // update-in-place keeps provenance
		rec.VerifiedAt, rec.VerifiedBy = old.VerifiedAt, old.VerifiedBy
		updating = true
	}
	if err := validateRecord(rec); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if !updating { // dupe defense: search-before-write made mechanical
		for _, sim := range similarRecords(dir, rec) {
			fmt.Fprintf(os.Stderr, "similar existing record: %s — %s (update it instead?)\n", sim.ID, sim.Title)
		}
	}
	if err := writeRecord(dir, rec); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	gitCommit(dir, "upsert("+*id+") via cli")
	fmt.Println("wrote", dirJoin(dir, *id+".md"))
}

func memGet(dir string, args []string) {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "")
	pos, flagArgs := splitPositional(args)
	fs.Parse(flagArgs)
	if len(pos) < 1 {
		usage()
		os.Exit(2)
	}
	r, err := readRecord(dirJoin(dir, pos[0]+".md"))
	if err != nil { // review flow: proposals must be readable BEFORE promotion
		if pr, perr := readRecord(dirJoin(dir, "pending", pos[0]+".md")); perr == nil {
			pr.Pending = true
			r, err = pr, nil
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	logRecall(dir, "get", pos[0], []string{r.ID})
	if *asJSON {
		printJSON(struct {
			Record
			Body string `json:"body"`
		}{r, r.Body})
		return
	}
	badge := ""
	if r.Pending {
		badge = "  [PENDING — promote to make injectable]"
	}
	fmt.Printf("# %s — %s%s\ncategory: %s  updated: %s  source: %s  verified: %s\ntags: %s\naliases: %s\n\n%s\n",
		r.ID, r.Title, badge, r.Category, shortDate(r.Updated), r.Source, orDash(strings.TrimSpace(r.VerifiedAt+" "+r.VerifiedBy)),
		strings.Join(r.Tags, ", "), strings.Join(r.Aliases, ", "), r.Body)
}

func memList(dir string, args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "")
	pendingOnly := fs.Bool("pending", false, "list the proposals inbox (pending/) instead of the store")
	fs.Parse(args)
	var recs []Record
	if *pendingOnly {
		for _, r := range loadAllRecords(dir) {
			if r.Pending {
				recs = append(recs, r)
			}
		}
	} else {
		recs = loadRecords(dir)
	}
	if *asJSON {
		if recs == nil {
			recs = []Record{} // JSON contract: empty array, never null
		}
		printJSON(recs)
		return
	}
	for _, r := range recs {
		badge := ""
		if r.Pending {
			badge = " [pending]"
		}
		fmt.Printf("%-40s %-13s %s  %s%s\n", r.ID, r.Category, shortDate(r.Updated), r.Title, badge)
	}
}

func memPromote(dir string, args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	if err := promoteRecord(dir, args[0]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("promoted to store (now injectable):", args[0])
}

func memReject(dir string, args []string) {
	fs := flag.NewFlagSet("reject", flag.ExitOnError)
	reason := fs.String("reason", "", "why (kept with the record as synthesis training data)")
	pos, flagArgs := splitPositional(args)
	fs.Parse(flagArgs)
	if len(pos) < 1 || *reason == "" {
		fmt.Fprintln(os.Stderr, "usage: cpmem memory reject <id> --reason \"...\"")
		os.Exit(2)
	}
	if err := rejectRecord(dir, pos[0], *reason); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("rejected (kept in rejected/):", pos[0])
}

func memDelete(dir string, args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	if err := deleteRecord(dir, args[0]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("deleted + tombstoned:", args[0])
	fmt.Println("(removed from store and index; retained in local git history until purge exists)")
}

func memSearch(dir string, args []string) {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	why := fs.Bool("why", false, "show which field matched (alias-tuning feedback)")
	category := fs.String("category", "", "facet filter")
	repo := fs.String("repository", "", "facet filter")
	asJSONSearch := fs.Bool("json", false, "")
	pos, flagArgs := splitPositional(args)
	fs.Parse(flagArgs)
	if len(pos) < 1 {
		usage()
		os.Exit(2)
	}
	query := strings.Join(pos, " ")
	hits := searchRecords(dir, query, SearchOpts{Category: *category, Repository: *repo})
	var ids []string
	for _, h := range hits {
		ids = append(ids, h.ID)
	}
	logRecall(dir, "search", query, ids)
	if *asJSONSearch {
		if hits == nil {
			hits = []Hit{} // JSON contract: empty array, never null
		}
		printJSON(hits) // includes Why[] — GUI renders match provenance (R2)
		return
	}
	if len(hits) == 0 {
		fmt.Println("no hits (logged — misses are the vector-gate evidence)")
		return
	}
	for _, h := range hits {
		badge := ""
		if h.Pending {
			badge = " [pending]"
		}
		fmt.Printf("%2d  %-40s %s%s\n", h.Score, h.ID, h.Title, badge)
		if *why {
			fmt.Printf("      matched: %s\n", strings.Join(h.Why, ", "))
		}
	}
}

func memIndex(dir string, args []string) {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	maxBytes := fs.Int("max-bytes", 4096, "index budget")
	project := fs.String("project", "", "repository filter")
	fs.Parse(args)
	block := buildIndex(dir, *maxBytes, *project)
	// Hook invocations pass a JSON payload on stdin. Output contract differs
	// per runtime (probed 2026-07-16): Claude injects plain stdout; Codex RUNS
	// the hook but DISCARDS plain stdout — it requires the structured envelope
	// (hookSpecificOutput.additionalContext). Discriminator: transcript_path.
	if hp := readHookPayload(); hp != nil && strings.Contains(hp.TranscriptPath, "/.codex/") {
		printJSON(map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "SessionStart",
				"additionalContext": block,
			},
		})
		return
	}
	fmt.Print(block)
}

func memLog(dir string, args []string) {
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	limit := fs.Int("limit", 20, "")
	fs.Parse(args)
	showRecallLog(*limit)
}

func memEdit(dir string, args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	path := dirJoin(dir, args[0]+".md")
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintln(os.Stderr, "no such record:", args[0])
		os.Exit(1)
	}
	if err := openEditor(path); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	r, err := readRecord(path)
	if err == nil {
		err = validateRecord(r)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "INVALID after edit (file left as-is, re-run edit to fix): %v\n", err)
		os.Exit(1)
	}
	r.Updated = time.Now().UTC().Format(time.RFC3339)
	if err := writeRecord(dir, r); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	gitCommit(dir, "edit("+r.ID+") via cli")
	fmt.Println("updated", path)
}

func memNew(dir string, args []string) {
	id := "new-memory-" + time.Now().UTC().Format("20060102-1504")
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id = args[0]
	}
	path := dirJoin(dir, id+".md")
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintln(os.Stderr, "record exists — use: cpmem memory edit", id)
		os.Exit(1)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	template := Record{ID: id, Title: "TITLE — put the identifiers here", Category: "note",
		Source: "human", Created: now, Updated: now,
		Body: "<!-- checklist: identifiers in the TITLE; fill ALIASES with every spelling\n" +
			"     (ch8 / channel-8 / channel 8); this is a TOPIC DOSSIER — update it in\n" +
			"     place as the topic evolves; body = current state + dated status lines;\n" +
			"     relational facts -> markdown TABLE; flows/topology -> mermaid fence;\n" +
			"     never prose-ify a matrix -->\n\n" +
			"(the fact/dossier body)"}
	if err := writeRecord(dir, template); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := openEditor(path); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	r, err := readRecord(path)
	if err == nil {
		err = validateRecord(r)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "INVALID (file kept, fix with: cpmem memory edit %s): %v\n", id, err)
		os.Exit(1)
	}
	for _, sim := range similarRecords(dir, r) {
		fmt.Fprintf(os.Stderr, "similar existing record: %s — %s (merge instead?)\n", sim.ID, sim.Title)
	}
	if err := writeRecord(dir, r); err != nil {
		fmt.Fprintln(os.Stderr, "write failed:", err)
		os.Exit(1)
	}
	gitCommit(dir, "new("+r.ID+") via cli")
	fmt.Println("created", path)
}

func memNote(dir string, args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	text := strings.Join(args, " ")
	// quick capture → pending/: unreviewed text must not silently enter the
	// injected index; curation (promote) is a deliberate act.
	id := "note-" + time.Now().UTC().Format("20060102-150405")
	now := time.Now().UTC().Format(time.RFC3339)
	pendingDir := dirJoin(dir, "pending")
	rec := Record{ID: id, Title: firstLine(text, 80), Category: "note",
		Source: "human", Created: now, Updated: now, Body: text}
	if err := writeRecord(pendingDir, rec); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	gitCommit(dir, "note("+id+") via cli")
	fmt.Println("captured to pending/ (findable in search as [pending]; promote when curated):", id)
}

func memVerify(dir string, args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	r, err := readRecord(dirJoin(dir, args[0]+".md"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	who := os.Getenv("USER")
	if who == "" {
		who = "unknown"
	}
	r.VerifiedAt = time.Now().UTC().Format("2006-01-02")
	r.VerifiedBy = who
	if err := writeRecord(dir, r); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	gitCommit(dir, "verify("+r.ID+") via cli")
	fmt.Printf("verified: %s (%s by %s)\n", r.ID, r.VerifiedAt, r.VerifiedBy)
}
