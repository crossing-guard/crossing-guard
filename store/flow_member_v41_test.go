package store

import (
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ncruces/go-sqlite3/driver"
)

// Schema 41 (flow-member-exclusion-class plan; planned as 40, renumbered at merge): the member table's CHECK is
// the class vocabulary the daemon writes. The literals below are the stored
// values on purpose — the test pins what lands in the column.

func enableTestFlow(t *testing.T, ix *Index, flowID string) {
	t.Helper()
	if err := ix.EnableFlow(flowID, []byte(`{"id":"`+flowID+`"}`), 100); err != nil {
		t.Fatal(err)
	}
}

func memberClasses(t *testing.T, ix *Index, flowID string) map[string]string {
	t.Helper()
	members, err := ix.FlowMembers(flowID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, member := range members {
		out[member.SessionID] = member.Excluded
	}
	return out
}

// A-1: the initial evaluation's writer accepts a no-stage member beside a
// placed one (fails on 62a2285 with the v39 CHECK).
func TestReplaceFlowMembersRecordsNoStage(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	enableTestFlow(t, ix, "f")
	members := []OrchestrationFlowMember{
		{FlowID: "f", Runtime: "claude", SessionID: "placed", Stage: "building", AdmittedAt: 110, UpdatedAt: 110},
		{FlowID: "f", Runtime: "claude", SessionID: "unplaced", Excluded: "no-stage",
			ExclusionReason: "no stage's membership query matches this session's current facts", AdmittedAt: 110, UpdatedAt: 110},
	}
	if err := ix.ReplaceFlowMembers("f", members, 110); err != nil {
		t.Fatalf("a no-stage member must be storable: %v", err)
	}
	got := memberClasses(t, ix, "f")
	if len(got) != 2 || got["placed"] != "" || got["unplaced"] != "no-stage" {
		t.Fatalf("members = %v", got)
	}
}

// A-2: the live writer folds a member between no-stage and placed.
func TestUpsertFlowMemberRecordsNoStage(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	enableTestFlow(t, ix, "f")
	member := OrchestrationFlowMember{FlowID: "f", Runtime: "claude", SessionID: "s", Excluded: "no-stage",
		ExclusionReason: "no stage matches", AdmittedAt: 110}
	for step, want := range []string{"no-stage", "", "no-stage"} {
		member.Excluded = want
		member.Stage = ""
		if want == "" {
			member.Stage = "building"
		}
		if err := ix.UpsertFlowMember(member, int64(120+step)); err != nil {
			t.Fatalf("step %d (%q): %v", step, want, err)
		}
		if got := memberClasses(t, ix, "f")["s"]; got != want {
			t.Fatalf("step %d: class = %q, want %q", step, got, want)
		}
	}
}

// A-3: every class the Go owner lists is storable; nothing else is.
func TestFlowMemberCheckIsTheExportedVocabulary(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	enableTestFlow(t, ix, "f")
	classes := FlowMemberExclusionClasses()
	if len(classes) != 4 || classes[0] != "" {
		t.Fatalf("vocabulary = %q", classes)
	}
	for _, class := range classes {
		member := OrchestrationFlowMember{FlowID: "f", Runtime: "claude", SessionID: "s-" + class, Excluded: class, AdmittedAt: 1}
		if err := ix.UpsertFlowMember(member, 2); err != nil {
			t.Fatalf("class %q refused: %v", class, err)
		}
	}
	for _, retired := range []string{"no-root", "bogus", "No-Stage"} {
		member := OrchestrationFlowMember{FlowID: "f", Runtime: "claude", SessionID: "r", Excluded: retired, AdmittedAt: 1}
		if err := ix.UpsertFlowMember(member, 2); err == nil || !strings.Contains(err.Error(), "CHECK") {
			t.Fatalf("class %q must be refused by the CHECK, got %v", retired, err)
		}
	}
	// The accessor hands out a copy: mutating it cannot change the owner.
	classes[3] = "mutated"
	if FlowMemberExclusionClasses()[3] == "mutated" {
		t.Fatal("FlowMemberExclusionClasses exposed its backing array")
	}
}

// v39MemberDDL is the member table exactly as aa0ee47 created it.
const v39MemberDDL = `CREATE TABLE orchestration_flow_member(
  flow_id TEXT NOT NULL REFERENCES orchestration_flow(flow_id) ON DELETE CASCADE,
  runtime TEXT NOT NULL,
  session_id TEXT NOT NULL,
  stage TEXT NOT NULL,
  excluded TEXT NOT NULL DEFAULT '' CHECK(excluded IN ('','shared-checkout','task-owned','no-root')),
  exclusion_reason TEXT NOT NULL DEFAULT '',
  bound INTEGER NOT NULL DEFAULT 0 CHECK(bound IN (0,1)),
  admitted_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(flow_id,runtime,session_id)
)`

// buildV39FlowStore creates a store, swaps the member table back to its v39
// layout, seeds it with extra SQL, and stamps 39.
func buildV39FlowStore(t *testing.T, seed string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	enableTestFlow(t, ix, "f")
	if _, err := ix.db.Exec(`DROP TABLE orchestration_flow_member; ` + v39MemberDDL + `; ` + seed +
		`; PRAGMA user_version = 39`); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func rawStoreRead(t *testing.T, path, query string, dest ...any) {
	t.Helper()
	db, err := driver.Open("file:" + path + "?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(query).Scan(dest...); err != nil {
		t.Fatal(err)
	}
}

func memberTableSQL(t *testing.T, db *sql.DB) (string, int64) {
	t.Helper()
	var ddl string
	var rootpage int64
	if err := db.QueryRow(`SELECT sql,rootpage FROM sqlite_master WHERE type='table' AND name='orchestration_flow_member'`).
		Scan(&ddl, &rootpage); err != nil {
		t.Fatal(err)
	}
	return ddl, rootpage
}

// A-4: a v39 store rebuilds once, keeps every row, matches a fresh store's
// DDL, and a second open is a no-op.
func TestV41RebuildsTheV39MemberTable(t *testing.T) {
	path := buildV39FlowStore(t, `INSERT INTO orchestration_flow_member(flow_id,runtime,session_id,stage,excluded,exclusion_reason,bound,admitted_at,updated_at) VALUES
  ('f','claude','a','building','','',1,10,11),
  ('f','claude','b','','shared-checkout','sibling open',0,12,13),
  ('f','codex','c','','task-owned','task owns it',0,14,15)`)
	ix, err := Open(path)
	if err != nil {
		t.Fatalf("v39 → v41 open: %v", err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	members, err := ix.FlowMembers("f")
	if err != nil {
		t.Fatal(err)
	}
	want := []OrchestrationFlowMember{
		{FlowID: "f", Runtime: "claude", SessionID: "a", Stage: "building", Bound: true, AdmittedAt: 10, UpdatedAt: 11},
		{FlowID: "f", Runtime: "claude", SessionID: "b", Excluded: "shared-checkout", ExclusionReason: "sibling open", AdmittedAt: 12, UpdatedAt: 13},
		{FlowID: "f", Runtime: "codex", SessionID: "c", Excluded: "task-owned", ExclusionReason: "task owns it", AdmittedAt: 14, UpdatedAt: 15},
	}
	if len(members) != len(want) {
		t.Fatalf("members = %+v", members)
	}
	for i := range want {
		if members[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v", i, members[i], want[i])
		}
	}
	migrated, rootpage := memberTableSQL(t, ix.db)
	fresh := openOwnerTagTestIndex(t)
	freshDDL, _ := memberTableSQL(t, fresh.db)
	if migrated != freshDDL {
		t.Fatalf("migrated DDL differs from a fresh store's:\n%s\n---\n%s", migrated, freshDDL)
	}
	if strings.Contains(migrated, "no-root") {
		t.Fatalf("retired class survived: %s", migrated)
	}
	if err := ix.UpsertFlowMember(OrchestrationFlowMember{FlowID: "f", Runtime: "claude", SessionID: "d",
		Excluded: "no-stage", AdmittedAt: 20}, 20); err != nil {
		t.Fatalf("no-stage after rebuild: %v", err)
	}
	rows, err := ix.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	violation := rows.Next()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if violation {
		t.Fatal("foreign_key_check reports a violation after the rebuild")
	}
	var version int
	if err := ix.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("user_version = %d (%v)", version, err)
	}
	// The cascade still holds on the rebuilt child.
	if _, err := ix.db.Exec(`DELETE FROM orchestration_flow WHERE flow_id='f'`); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := ix.db.QueryRow(`SELECT count(*) FROM orchestration_flow_member`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("cascade left %d members (%v)", left, err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen: the probe sees the current CHECK and rebuilds nothing.
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close() })
	if _, rootpageAgain := memberTableSQL(t, again.db); rootpageAgain != rootpage {
		t.Fatalf("second open rebuilt the table: rootpage %d → %d", rootpage, rootpageAgain)
	}
}

// A-5 (D-3): a row in a retired class refuses the migration; the store is
// left at v39 with the row intact, and the error names the remedy.
func TestV41RefusesARetiredClassRow(t *testing.T) {
	path := buildV39FlowStore(t, `INSERT INTO orchestration_flow_member(flow_id,runtime,session_id,stage,excluded,admitted_at,updated_at)
  VALUES('f','claude','x','','no-root',1,1)`)
	_, err := Open(path)
	if err == nil {
		t.Fatal("a retired-class row must refuse the migration")
	}
	for _, want := range []string{"no-root", "1 member rows", "DELETE FROM orchestration_flow_member", "re-apply"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
	var version int
	rawStoreRead(t, path, `PRAGMA user_version`, &version)
	var class, ddl string
	rawStoreRead(t, path, `SELECT excluded FROM orchestration_flow_member WHERE session_id='x'`, &class)
	rawStoreRead(t, path, `SELECT sql FROM sqlite_master WHERE name='orchestration_flow_member'`, &ddl)
	if version != 39 || class != "no-root" || ddl != v39MemberDDL {
		t.Fatalf("store changed by a refused migration: version=%d class=%q ddl=%s", version, class, ddl)
	}
}

// Every FlowMemberExcluded* constant declared in the store is in the list
// the CHECK is generated from, and every class has the shape the generator
// relies on (no quoting beyond the wrapping quotes).
func TestFlowMemberConstantsAreAllInTheVocabulary(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "session_owner_tag_journal.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	classes := FlowMemberExclusionClasses()
	found := 0
	for _, decl := range file.Decls {
		general, ok := decl.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, spec := range general.Specs {
			value := spec.(*ast.ValueSpec)
			for index, name := range value.Names {
				if !strings.HasPrefix(name.Name, "FlowMemberExcluded") {
					continue
				}
				literal, ok := value.Values[index].(*ast.BasicLit)
				if !ok {
					t.Fatalf("%s is not a string literal constant", name.Name)
				}
				class, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal(err)
				}
				found++
				if !slices.Contains(classes, class) {
					t.Errorf("%s = %q is not in FlowMemberExclusionClasses, so the CHECK refuses it", name.Name, class)
				}
			}
		}
	}
	if found != len(classes)-1 {
		t.Fatalf("found %d FlowMemberExcluded* constants for %d non-empty classes", found, len(classes)-1)
	}
	shape := regexp.MustCompile(`^[a-z-]*$`)
	for _, class := range classes {
		if !shape.MatchString(class) {
			t.Errorf("class %q does not match [a-z-]*", class)
		}
	}
}

// The rebuild copies flowMemberColumns; it must be the live table's full
// column list, or a column added later would be dropped by a rebuild.
func TestFlowMemberRebuildCopiesEveryColumn(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	rows, err := ix.db.Query(`SELECT name FROM pragma_table_info('orchestration_flow_member') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names, ","); got != flowMemberColumns {
		t.Fatalf("table columns %s != rebuild columns %s", got, flowMemberColumns)
	}
}
