package memcli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/memory"
	"crossing-guard/store"
	"crossing-guard/teamwire"
)

// cmdMemory dispatches `memory <verb>` to a per-verb helper. Since the
// first-class-records change every verb goes through the ONE write owner in
// crossing-guard/store; the files directory is a write-through mirror the verbs
// maintain after commit. Each verb is still its own nameable job.
func cmdMemory(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	rest := args[1:]
	switch args[0] {
	case "upsert":
		memUpsert(rest)
	case "get":
		memGet(rest)
	case "list":
		memList(rest)
	case "promote":
		memPromote(rest)
	case "reject":
		memReject(rest)
	case "delete":
		memDelete(rest)
	case "search":
		memSearch(rest)
	case "index":
		memIndex(rest)
	case "log":
		memLog(rest)
	case "edit":
		memEdit(rest)
	case "new":
		memNew(rest)
	case "note":
		memNote(rest)
	case "verify":
		memVerify(rest)
	case "take-team-version":
		memTakeTeamVersion(rest)
	case "migrate-files":
		memMigrateFilesCmd(rest)
	case "import-file":
		memImportFile(rest)
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

// parseSources parses repeatable `--source vendor:session-id[#anchor[:kind]]`
// flags (plan §4.2: proposals must cite evidence; human records may).
func parseSourceFlags(raw []string) ([]store.MemorySource, error) {
	var out []store.MemorySource
	for _, s := range raw {
		vendor, rest, ok := strings.Cut(s, ":")
		if !ok || vendor == "" || rest == "" {
			return nil, fmt.Errorf("source %q must be vendor:session-id[#anchor[:kind]]", s)
		}
		if !slices.Contains(harvest.RuntimeNames(), vendor) {
			return nil, fmt.Errorf("source vendor %q not recognized (%s)", vendor, strings.Join(harvest.RuntimeNames(), "|"))
		}
		src := store.MemorySource{Vendor: vendor, SessionID: rest, AnchorKind: "none"}
		if session, anchor, ok := strings.Cut(rest, "#"); ok {
			src.SessionID = session
			src.Anchor, src.AnchorKind = anchor, "event-uuid"
			if anchor, kind, ok := strings.Cut(anchor, ":"); ok {
				switch kind {
				case "event-uuid", "turn-id", "line-offset":
					src.Anchor, src.AnchorKind = anchor, kind
				default:
					return nil, fmt.Errorf("anchor kind %q not in enum (event-uuid|turn-id|line-offset)", kind)
				}
			}
		}
		out = append(out, src)
	}
	return out, nil
}

func memUpsert(args []string) {
	fs := flag.NewFlagSet("upsert", flag.ExitOnError)
	id := fs.String("id", "", "kebab-case slug (reuse = update in place)")
	title := fs.String("title", "", "")
	category := fs.String("category", "", categoriesHelp())
	tags := fs.String("tags", "", "comma-separated")
	aliases := fs.String("aliases", "", "comma-separated identifier spellings (v2, version-2, …)")
	repo := fs.String("repository", "", "")
	scope := fs.String("scope", "", "user|repository|organization (default: repository when --repository is set, else user)")
	source := fs.String("source", "human", "human|agent|harvest")
	content := fs.String("content", "", "body (else read from stdin)")
	sources := multiFlag(fs, "source-citation", "evidence: vendor:session-id[#anchor[:kind]], repeatable")
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
	citations, err := parseSourceFlags(*sources)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	rec := store.MemoryRecord{
		ID: *id, Title: *title, Category: *category,
		Tags: memory.SplitCSV(*tags), Aliases: memory.SplitCSV(*aliases),
		Source: *source, Body: body, Status: "active",
	}
	if rec.ScopeType, rec.ScopeID, rec.RepositoryIdentity, rec.IdentityNote, err = upsertScope(*scope, *repo); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	who := os.Getenv("USER")
	if who == "" {
		who = "local"
	}
	rec.AuthorType, rec.AuthorID = "user", who
	refused := lastRefusal(rec.ID) // before the write queues the next revision
	// Only `--scope user` in so many words narrows a team record (and detaches a copy,
	// O-11); a verb that names no scope updates the record where it is (FR-1).
	write := upsertThroughStore
	if *scope == string(store.MemoryScopeUser) {
		write = narrowThroughStore
	}
	saved, err := write(rec, citations)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (revision %d, scope %s)\n", saved.ID, saved.Revision, saved.ScopeType)
	switch {
	case saved.ID != rec.ID:
		// The store detached a team record narrowed to user scope (team item 5, O-11).
		fmt.Printf("(%s is the team's and is unchanged; %s is your own copy and stays on this device)\n", rec.ID, saved.ID)
	case store.MemoryShareable(saved) && refused != "":
		fmt.Printf("(queued for the team — but the team refused this record's last edit: %s. If it refuses this one too, the team keeps its own version; `crossing-guard memory take-team-version %s` brings that back here)\n", refusalWords(refused), saved.ID)
	case store.MemoryShareable(saved):
		fmt.Println("(shared with the linked team: it is sent on the next push)")
	case saved.ScopeType == store.MemoryScopeRepository && saved.RepositoryIdentity != "remote-sha256":
		fmt.Println("(stays on this device: " + saved.IdentityNote + ")")
	case saved.ScopeType != store.MemoryScopeUser && saved.Status == "active":
		fmt.Println("(not shared: share it from Settings → Team, or link this device first)")
	}
}

// upsertScope resolves the scope a written record gets from the verb's flags:
//   - organization: only on a linked device, and its id is the linked organization's —
//     from team.json, never from --repository (team item 5 decision 7);
//   - repository (named by --scope or by --repository alone): the identity is minted
//     from the current folder (decision 18) — a checkout with one origin remote keys the
//     record by that remote; anything else stays weak, by name, with the reason;
//   - otherwise user, or the scope as given.
func upsertScope(scope, repo string) (store.MemoryScopeType, string, string, string, error) {
	switch {
	case scope == string(store.MemoryScopeOrganization):
		orgID, err := linkedOrganizationID()
		return store.MemoryScopeOrganization, orgID, "", "", err
	case scope == string(store.MemoryScopeRepository) || (scope == "" && repo != ""):
		id, identity, note := mintRepositoryScope(repo)
		return store.MemoryScopeRepository, id, identity, note, nil
	case scope != "":
		return store.MemoryScopeType(scope), repo, "", "", nil
	}
	return store.MemoryScopeUser, "", "", "", nil
}

func memGet(args []string) {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "")
	pos, flagArgs := splitPositional(args)
	fs.Parse(flagArgs)
	if len(pos) < 1 {
		usage()
		os.Exit(2)
	}
	// The daemon reads the record and writes the recall log (via=cli).
	r, raw, err := daemonRecord(pos[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *asJSON {
		raw.print()
		return
	}
	sources := r.Sources
	badge := ""
	if r.Status == "pending" {
		badge = "  [PENDING — promote to make injectable]"
	}
	fmt.Printf("# %s — %s%s\nrevision %d  scope: %s  category: %s  updated: %s\nsource: %s  verified: %s\ntags: %s\naliases: %s\n\n%s\n",
		r.ID, r.Title, badge, r.Revision, r.ScopeType, r.Category, shortDate(r.Updated),
		r.Source, orDash(strings.TrimSpace(r.VerifiedAt+" "+r.VerifiedBy)),
		strings.Join(r.Tags, ", "), strings.Join(r.Aliases, ", "), r.Body)
	if len(sources) > 0 {
		fmt.Println("\ncited sessions:")
		for _, s := range sources {
			fmt.Printf("  %s/%s", s.Vendor, s.SessionID)
			if s.Anchor != "" {
				fmt.Printf(" (%s: %s)", s.AnchorKind, s.Anchor)
			}
			fmt.Println()
		}
	}
}

func memList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "")
	status := fs.String("status", "", "active|pending|rejected (default: active)")
	fs.Parse(args)
	if *status == "" {
		*status = "active"
	}
	recs, raw, err := daemonRecords(*status)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *asJSON {
		raw.print() // the daemon's records array; empty is [], never null
		return
	}
	for _, r := range recs {
		fmt.Printf("%-40s %-13s %-6s %s  %s\n", r.ID, r.Category, r.ScopeType,
			shortDate(r.Updated), r.Title)
	}
}

func memPromote(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	ix, err := memStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer ix.Close()
	r, err := ix.PromoteMemory(args[0], cliActor())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	mirrorRecord(r)
	fmt.Println("promoted to store (now injectable):", args[0])
}

func memReject(args []string) {
	fs := flag.NewFlagSet("reject", flag.ExitOnError)
	reason := fs.String("reason", "", "why (kept with the record as synthesis training data)")
	pos, flagArgs := splitPositional(args)
	fs.Parse(flagArgs)
	if len(pos) < 1 || *reason == "" {
		fmt.Fprintln(os.Stderr, "usage: crossing-guard memory reject <id> --reason \"...\"")
		os.Exit(2)
	}
	ix, err := memStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer ix.Close()
	r, err := ix.RejectMemory(pos[0], *reason, cliActor())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	mirrorRemove(r.ID)
	fmt.Println("rejected (kept in the store with its history):", pos[0])
}

func memDelete(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	ix, err := memStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer ix.Close()
	done, err := ix.DeleteMemoryReport(args[0], cliActor())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	mirrorRemove(args[0])
	fmt.Println("deleted + tombstoned:", args[0])
	// What the store did, said as it was (O-7): a record the team holds loses its
	// revision history too, and the deletion goes to the team only from a linked device.
	switch {
	case done.Team && done.Queued:
		fmt.Println("(a team record: removed from store, index and mirror with its revision history; the deletion is sent to the team on the next push)")
		return
	case done.Team:
		fmt.Println("(a team record: removed from store, index and mirror with its revision history; this device is not linked, so the team is not told)")
		return
	}
	fmt.Println("(removed from store, index and mirror; the revision history keeps the content until a purge exists)")
}

func memSearch(args []string) {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	why := fs.Bool("why", false, "show which field matched (alias-tuning feedback)")
	category := fs.String("category", "", "facet filter")
	repo := fs.String("repository", "", "facet filter")
	tag := fs.String("tag", "", "facet filter")
	limit := fs.Int("limit", 0, "at most N hits (default: the daemon's configured maximum)")
	asJSONSearch := fs.Bool("json", false, "")
	pos, flagArgs := splitPositional(args)
	fs.Parse(flagArgs)
	if (len(pos) < 1 && *tag == "") || *limit < 0 {
		usage()
		os.Exit(2)
	}
	query := strings.Join(pos, " ")
	// The daemon searches and writes the recall log (via=cli): one scorer.
	resp, raw, err := daemonSearch(query, *category, *repo, *tag, *limit)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *asJSONSearch {
		raw.print()
		return
	}
	hits := resp.Hits
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
	if resp.Truncated {
		hint := "narrow with --category, --repository or --tag"
		if len(hits) < resp.LimitMax {
			hint = fmt.Sprintf("--limit up to %d for more, or narrow with --category, --repository or --tag", resp.LimitMax)
		}
		fmt.Printf("showing %d of %d (%s)\n", len(hits), resp.Total, hint)
	}
}

func memIndex(args []string) {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	maxBytes := fs.Int("max-bytes", 4096, "index budget")
	project := fs.String("project", "", "repository filter")
	runtime := fs.String("runtime", "", "memory output adapter (otherwise legacy detection or plaintext)")
	fs.Parse(args)
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "runtime" {
			explicit = true
		}
	})
	var payload []byte
	if !explicit {
		payload = readHookPayload()
	}
	encoder, err := memoryIndexOutput(*runtime, explicit, payload)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// The injection block reads the STORE (plan §3.4); the mirror directory is
	// where the recall log and nonce contract live, unchanged.
	block := memIndexBlock(*maxBytes, *project)
	if encoder != nil {
		out, err := encoder.EncodeMemoryIndex(block)
		if err != nil {
			fmt.Fprintln(os.Stderr, "memory index:", err)
			os.Exit(1)
		}
		fmt.Print(string(out))
		return
	}
	fmt.Print(block)
}

func memLog(args []string) {
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	limit := fs.Int("limit", 20, "")
	fs.Parse(args)
	showRecallLog(*limit)
}

func memEdit(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	r, _, err := memGetRecord(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "no such record:", args[0])
		os.Exit(1)
	}
	// Edit through a temp file in the mirror's format, then adopt: the file's
	// shape is the editing UX; the store is truth (RT-7 — the edit is explicit,
	// so adoption is too).
	tmpRec := memory.RecordFromStore(r.ID, r.Status, string(r.ScopeType), r.ScopeID,
		r.Title, r.Category, r.Body, r.Tags, r.Aliases, r.Source, r.Origin,
		r.SupersededBy, r.VerifiedAt, r.VerifiedBy,
		fmtMemoryTime(r.CreatedAt), fmtMemoryTime(r.UpdatedAt))
	path := filepath.Join(os.TempDir(), args[0]+".md.edit")
	if err := writeRecord(os.TempDir(), tmpRec); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.Remove(path) }()
	if err := openEditor(path); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	edited, err := readRecord(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	updated := store.MemoryRecord{
		ID: edited.ID, Title: edited.Title, Category: edited.Category, Body: edited.Body,
		Tags: edited.Tags, Aliases: edited.Aliases, Source: r.Source, Status: r.Status,
		ScopeType: r.ScopeType, ScopeID: r.ScopeID, RepositoryIdentity: r.RepositoryIdentity,
		Origin: r.Origin, SupersededBy: edited.Superseded, VerifiedAt: r.VerifiedAt, VerifiedBy: r.VerifiedBy,
		AuthorType: "user", AuthorID: cliActor().AuthorID,
	}
	saved, err := reviseThroughStore(updated, r.Revision)
	if errors.Is(err, store.ErrMemoryStale) {
		fmt.Fprintf(os.Stderr, "not saved: %v\nRead the record again and re-apply the edit; nothing was written.\n", err)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "INVALID after edit (nothing written): %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("updated %s (revision %d)\n", saved.ID, saved.Revision)
	fmt.Println("(edits land in the store; the mirror is regenerated)")
}

func memNew(args []string) {
	id := store.NewMemoryID("new-memory")
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id = args[0]
	}
	if _, _, err := memGetRecord(id); err == nil {
		fmt.Fprintln(os.Stderr, "record exists — use: crossing-guard memory edit", id)
		os.Exit(1)
	}
	rec := store.MemoryRecord{
		ID: id, Title: "TITLE — put the identifiers here", Category: "note",
		Source: "human", Status: "active", ScopeType: store.MemoryScopeUser,
		AuthorType: "user", AuthorID: cliActor().AuthorID,
		Body: "<!-- checklist: identifiers in the TITLE; fill ALIASES with every spelling\n" +
			"     (v2 / version-2 / version 2); this is a TOPIC DOSSIER — update it in\n" +
			"     place as the topic evolves; body = current state + dated status lines;\n" +
			"     relational facts -> markdown TABLE; flows/topology -> mermaid fence;\n" +
			"     never prose-ify a matrix -->\n\n" +
			"(the fact/dossier body)",
	}
	if _, err := createThroughStore(rec, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Open the mirror file for editing, then re-adopt (same UX as edit).
	mirrorPath := filepath.Join(memory.DefaultDir(), id+".md")
	if err := openEditor(mirrorPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	edited, err := readRecord(mirrorPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "INVALID (fix with: crossing-guard memory edit", id, "):", err)
		os.Exit(1)
	}
	editedRec := store.MemoryRecord{
		ID: id, Title: edited.Title, Category: edited.Category, Body: edited.Body,
		Tags: edited.Tags, Aliases: edited.Aliases, Source: "human", Status: "active",
		ScopeType: store.MemoryScopeUser, AuthorType: "user", AuthorID: cliActor().AuthorID,
	}
	if _, err := upsertThroughStore(editedRec, nil); err != nil {
		fmt.Fprintln(os.Stderr, "write failed:", err)
		os.Exit(1)
	}
	fmt.Println("created", id)
}

func memNote(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	text := strings.Join(args, " ")
	// quick capture → pending: unreviewed text must not silently enter the
	// injected index; curation (promote) is a deliberate act.
	id := store.NewMemoryID("note")
	rec := store.MemoryRecord{
		ID: id, Title: firstLine(text, 80), Category: "note",
		Source: "human", Status: "pending", ScopeType: store.MemoryScopeUser,
		AuthorType: "user", AuthorID: cliActor().AuthorID, Body: text,
	}
	if _, err := createThroughStore(rec, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("captured to pending (findable in search; promote when curated):", id)
}

// memTakeTeamVersion gives up this device's edit of a team record; the daemon's next
// memory pull brings the team's current revision back and keeps the local body as a
// conflict copy (team item 5, FR-6).
func memTakeTeamVersion(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	ix, err := memStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer ix.Close()
	if err := ix.TakeTeamVersion(args[0]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%s: the edit made here is given up; the team's version returns with the next pull, and this device's body is kept as a conflict copy\n", args[0])
}

// lastRefusal is the code the team refused a record's latest revision with (” when it
// was not refused, the record does not exist, or the store cannot be read).
func lastRefusal(id string) string {
	ix, err := store.OpenRO(indexDB())
	if err != nil {
		return ""
	}
	defer ix.Close()
	r, err := ix.MemoryByID(id)
	if err != nil {
		return ""
	}
	code, _ := ix.MemoryRefusal(r.GlobalID)
	return code
}

// refusalWords says a refusal code in words, with the code.
func refusalWords(code string) string {
	switch code {
	case teamwire.CodeOrganizationScopeAdmin:
		return "only an owner or admin changes an organization record (" + code + ")"
	case teamwire.CodeSlugImmutable:
		return "a team record cannot be renamed (" + code + ")"
	case teamwire.CodeTombstoned:
		return "the record was deleted for the team (" + code + ")"
	}
	return code
}

func memVerify(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	r, _, err := memGetRecord(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	r.VerifiedAt = time.Now().UTC().Format("2006-01-02")
	r.VerifiedBy = cliActor().AuthorID
	saved, err := reviseThroughStore(r, r.Revision)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("verified: %s (%s by %s)\n", saved.ID, saved.VerifiedAt, saved.VerifiedBy)
}

func memMigrateFilesCmd(args []string) {
	st, imported, err := memMigrateFiles()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("migrated %d records into the store (%d store, %d pending, %d rejected, %d tombstones, %d errors)\n",
		imported, st.Store, st.Pending, st.Rejected, st.Tombstones, st.Errors)
	fmt.Println("(the files directory is now a mirror; archive the old git history with: mv ~/.crossing-guard/memory ~/.crossing-guard/memory-archive-git)")
}

func memImportFile(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	r, err := readRecord(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	rec := store.MemoryRecord{
		ID: r.ID, Title: r.Title, Category: r.Category, Body: r.Body,
		Tags: r.Tags, Aliases: r.Aliases, Source: "human", Status: "active",
		ScopeType: store.MemoryScopeUser, AuthorType: "user", AuthorID: cliActor().AuthorID,
		Origin: "hand-edit adoption via import-file",
	}
	if r.Repository != "" {
		rec.ScopeType, rec.ScopeID, rec.RepositoryIdentity = store.MemoryScopeRepository, r.Repository, "weak"
	}
	// A mirror file carries no scope type and no repository identity: adopting a hand
	// edit of an existing record keeps the scope the store holds for it (FR-1), so an
	// organization record stays the organization's and a remote-identified repository
	// record is not downgraded to a folder name.
	if cur, _, err := memGetRecord(r.ID); err == nil {
		rec.ScopeType, rec.ScopeID, rec.RepositoryIdentity, rec.IdentityNote = cur.ScopeType, cur.ScopeID, cur.RepositoryIdentity, cur.IdentityNote
	}
	saved, err := upsertThroughStore(rec, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("adopted %s from %s (revision %d)\n", saved.ID, args[0], saved.Revision)
}

// multiFlag registers a repeatable string flag (flag.Value).
func multiFlag(fs *flag.FlagSet, name, help string) *[]string {
	var out []string
	fs.Var(&repeatableString{&out}, name, help)
	return &out
}

type repeatableString struct{ vals *[]string }

// String is also called by the flag package on a ZERO repeatableString, to tell whether
// a flag's value is its default when it prints the usage: there are no values then.
func (r *repeatableString) String() string {
	if r == nil || r.vals == nil {
		return ""
	}
	return strings.Join(*r.vals, ", ")
}
func (r *repeatableString) Set(v string) error {
	*r.vals = append(*r.vals, v)
	return nil
}
