package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
)

// openForTest opens the file read-only with no schema work, to look at a store a
// migration refused.
func openForTest(path string) (*sql.DB, error) {
	return driver.Open("file:"+path+"?mode=ro", fts5.Register)
}

// downgradeUnderstandingShapeToV44 rewrites a schema-45 store's unit and edge tables
// into the pre-45 layout, rows kept: descriptor text inline, an edge table with a
// rowid, both edge indexes.
func downgradeUnderstandingShapeToV44(t *testing.T, ix *Index) {
	t.Helper()
	if _, err := ix.db.Exec(`
DROP TRIGGER understanding_unit_complete;
DROP INDEX understanding_unit_descriptor;
ALTER TABLE understanding_unit RENAME TO understanding_unit_v44;
CREATE TABLE understanding_unit(
  generation_id INTEGER NOT NULL REFERENCES understanding_generation(id) ON DELETE RESTRICT,
  path TEXT NOT NULL,
  source_hash TEXT NOT NULL CHECK(source_hash LIKE 'sha256-v1:%'),
  language TEXT NOT NULL DEFAULT '',
  namespace TEXT NOT NULL DEFAULT '',
  descriptor_json TEXT NOT NULL,
  PRIMARY KEY(generation_id,path)
);
INSERT INTO understanding_unit SELECT unit.generation_id,unit.path,unit.source_hash,unit.language,unit.namespace,descriptor.descriptor_json
  FROM understanding_unit_v44 unit JOIN understanding_descriptor descriptor ON descriptor.digest=unit.descriptor_digest;
DROP TABLE understanding_unit_v44;
DELETE FROM understanding_descriptor;
CREATE TRIGGER understanding_unit_complete BEFORE INSERT ON understanding_unit
BEGIN
  SELECT CASE WHEN COALESCE((SELECT status FROM understanding_generation WHERE id=NEW.generation_id),'')!='complete'
    THEN RAISE(ABORT,'understanding units require a complete generation') END;
END;
DROP TRIGGER understanding_edge_complete;
DROP INDEX understanding_edge_to;
ALTER TABLE understanding_edge RENAME TO understanding_edge_v44;
` + strings.Replace(understandingEdgeTableV45, ") WITHOUT ROWID", ")", 1) + `;
INSERT INTO understanding_edge SELECT * FROM understanding_edge_v44;
DROP TABLE understanding_edge_v44;
CREATE INDEX understanding_edge_from ON understanding_edge(generation_id,from_kind,from_ref);
CREATE INDEX understanding_edge_to ON understanding_edge(generation_id,to_kind,to_ref);
CREATE TRIGGER understanding_edge_complete BEFORE INSERT ON understanding_edge
BEGIN
  SELECT CASE WHEN COALESCE((SELECT status FROM understanding_generation WHERE id=NEW.generation_id),'')!='complete'
    THEN RAISE(ABORT,'understanding edges require a complete generation') END;
END;
PRAGMA user_version=44`); err != nil {
		t.Fatal(err)
	}
}

// reopenRebuilding opens a store whose understanding tables hold rows in an older
// layout the way its owner does: an ordinary Open must refuse to start the rebuild,
// the exclusive open performs it, and an ordinary Open then succeeds.
func reopenRebuilding(t *testing.T, path string) *Index {
	t.Helper()
	if readOnly, err := OpenRO(path); !errors.Is(err, ErrUnderstandingRebuildPending) {
		if readOnly != nil {
			readOnly.Close()
		}
		t.Fatalf("a read-only open of a store awaiting the rebuild: %v", err)
	}
	if refused, err := Open(path); !errors.Is(err, ErrUnderstandingRebuildPending) {
		if refused != nil {
			refused.Close()
		}
		t.Fatalf("an ordinary open of a store awaiting the rebuild: %v", err)
	}
	ix, err := OpenAsOwner(path)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// shapeGeneration is a generation with three units, two of which carry descriptor
// text that every other shapeGeneration shares, and edges reachable from both ends.
func shapeGeneration(t *testing.T, ix *Index, digest string, started int64) UnderstandingGeneration {
	t.Helper()
	g := completeUnderstanding()
	g.SnapshotDigest = "git-tree-v2-sha256:" + digest
	g.StartedAt, g.EndedAt = started, started+1
	g.Units = []UnderstandingUnit{
		{Path: "a.go", SourceHash: "sha256-v1:a", Language: "go", Namespace: "example/a", DescriptorJSON: `{"path":"a.go","shared":true}`},
		{Path: "b.go", SourceHash: "sha256-v1:b", Language: "go", Namespace: "example/a", DescriptorJSON: `{"path":"b.go","shared":true}`},
		{Path: "c.go", SourceHash: "sha256-v1:" + digest, Language: "go", Namespace: "example/a", DescriptorJSON: fmt.Sprintf(`{"path":"c.go","only":%q}`, digest)},
	}
	g.Edges = []UnderstandingEdge{
		{FromKind: "file", FromRef: "a.go", Relation: "file_in_package", ToKind: "package", ToRef: "example/a", SourcePath: "a.go", SourceLine: 1, Provenance: "measured", AnalyzerID: "go-ast-v1", EvidenceDigest: "sha256-v1:1"},
		{FromKind: "file", FromRef: "a.go", Relation: "file_declares_symbol", ToKind: "symbol", ToRef: "go::A", SourcePath: "a.go", SourceLine: 3, Provenance: "measured", AnalyzerID: "go-ast-v1", EvidenceDigest: "sha256-v1:2"},
		{FromKind: "symbol", FromRef: "go::B", Relation: "symbol_calls_symbol", ToKind: "symbol", ToRef: "go::A", SourcePath: "b.go", SourceLine: 9, Provenance: "measured", AnalyzerID: "go-ast-v1", EvidenceDigest: "sha256-v1:3"},
		{FromKind: "package", FromRef: "example/b", Relation: "package_depends_on", ToKind: "package", ToRef: "example/a", SourcePath: "b/b.go", SourceLine: 4, Provenance: "measured", AnalyzerID: "go-ast-v1", EvidenceDigest: "sha256-v1:4"},
	}
	if err := ix.AppendUnderstanding(&g); err != nil {
		t.Fatal(err)
	}
	return g
}

type shapeReads struct {
	Units      []UnderstandingUnit
	ByPath     []UnderstandingUnit
	Index      []UnderstandingUnit
	All        []UnderstandingEdge
	FromFile   []UnderstandingEdge
	ToSymbol   []UnderstandingEdge
	Callers    []UnderstandingEdge
	Dependents UnderstandingDependencyValues
}

func readShape(t *testing.T, ix *Index, generationID int64) shapeReads {
	t.Helper()
	var out shapeReads
	var err error
	check := func() {
		if err != nil {
			t.Fatal(err)
		}
	}
	out.Units, _, err = ix.UnderstandingUnits(generationID, 0, 100)
	check()
	out.ByPath, err = ix.UnderstandingUnitsForPaths(generationID, []string{"a.go", "c.go"})
	check()
	out.Index, err = ix.UnderstandingUnitIndex(generationID)
	check()
	out.All, _, err = ix.UnderstandingEdges(generationID, "", "", 0, 100)
	check()
	out.FromFile, _, err = ix.UnderstandingEdges(generationID, "file", "a.go", 0, 100)
	check()
	out.ToSymbol, _, err = ix.UnderstandingEdges(generationID, "symbol", "go::A", 0, 100)
	check()
	out.Callers, _, err = ix.UnderstandingCallers(generationID, []string{"go::A"}, 0, 100)
	check()
	out.Dependents, err = ix.UnderstandingDependentPackageValues(generationID, []string{"a.go"}, 100)
	check()
	return out
}

func understandingShapeLayout(t *testing.T, ix *Index) string {
	t.Helper()
	var layout strings.Builder
	for _, table := range []string{"understanding_unit", "understanding_edge", "understanding_descriptor"} {
		rows, err := ix.db.Query(`SELECT name||'|'||type||'|'||"notnull"||'|'||COALESCE(dflt_value,'')||'|'||pk FROM pragma_table_info(?) ORDER BY cid`, table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var column string
			if err := rows.Scan(&column); err != nil {
				t.Fatal(err)
			}
			layout.WriteString(table + ":" + column + "\n")
		}
		rows.Close()
	}
	// Index and trigger definitions, with whitespace and IF NOT EXISTS folded: the
	// base DDL and the migration spell the same objects slightly differently.
	rows, err := ix.db.Query(`SELECT type||' '||name||' '||COALESCE(sql,'') FROM sqlite_master
		WHERE tbl_name IN ('understanding_unit','understanding_edge','understanding_descriptor') AND type IN ('index','trigger')
		ORDER BY type,name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var definition string
		if err := rows.Scan(&definition); err != nil {
			t.Fatal(err)
		}
		definition = strings.ReplaceAll(definition, "IF NOT EXISTS ", "")
		layout.WriteString(strings.Join(strings.Fields(definition), " ") + "\n")
	}
	// The table definitions themselves, whitespace folded: the base DDL and the
	// migration's constants are two copies of one text and must not drift.
	tables, err := ix.db.Query(`SELECT name||' '||sql FROM sqlite_master WHERE type='table'
		AND name IN ('understanding_unit','understanding_edge','understanding_descriptor') ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer tables.Close()
	for tables.Next() {
		var definition string
		if err := tables.Scan(&definition); err != nil {
			t.Fatal(err)
		}
		definition = strings.ReplaceAll(definition, "IF NOT EXISTS ", "")
		layout.WriteString(strings.Join(strings.Fields(definition), " ") + "\n")
	}
	var withoutRowid bool
	if err := ix.db.QueryRow(`SELECT sql LIKE '%WITHOUT ROWID%' FROM sqlite_master WHERE name='understanding_edge'`).Scan(&withoutRowid); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&layout, "edge without rowid: %v\n", withoutRowid)
	return layout.String()
}

func TestV45UpgradeStoresDescriptorsOnceAndKeepsEveryRead(t *testing.T) {
	fresh, err := Open(filepath.Join(t.TempDir(), "fresh.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	freshLayout := understandingShapeLayout(t, fresh)
	if !strings.Contains(freshLayout, "edge without rowid: true") || strings.Contains(freshLayout, "understanding_edge_from") ||
		!strings.Contains(freshLayout, "understanding_unit_descriptor") {
		t.Fatalf("fresh layout:\n%s", freshLayout)
	}

	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	generations := []UnderstandingGeneration{shapeGeneration(t, ix, "one", 10), shapeGeneration(t, ix, "two", 20), shapeGeneration(t, ix, "three", 30)}
	before := map[int64]shapeReads{}
	for _, generation := range generations {
		before[generation.ID] = readShape(t, ix, generation.ID)
	}
	downgradeUnderstandingShapeToV44(t, ix)
	ix.Close()

	// Two rows a batch, so the nine-row unit copy crosses four batch boundaries.
	previousBatch := understandingShapeBatch
	understandingShapeBatch = 2
	t.Cleanup(func() { understandingShapeBatch = previousBatch })
	ix = reopenRebuilding(t, path)
	defer ix.Close()
	if layout := understandingShapeLayout(t, ix); layout != freshLayout {
		t.Fatalf("migrated layout differs from fresh:\n%s\n--- fresh ---\n%s", layout, freshLayout)
	}
	for _, generation := range generations {
		if after := readShape(t, ix, generation.ID); !reflect.DeepEqual(before[generation.ID], after) {
			t.Fatalf("generation %d reads changed across the migration:\nbefore %+v\nafter  %+v", generation.ID, before[generation.ID], after)
		}
	}
	// Nine unit rows, five distinct texts: two shared by all three, one each.
	var descriptors, units int
	if err := ix.db.QueryRow(`SELECT (SELECT COUNT(*) FROM understanding_descriptor),(SELECT COUNT(*) FROM understanding_unit)`).Scan(&descriptors, &units); err != nil || descriptors != 5 || units != 9 {
		t.Fatalf("descriptors=%d units=%d err=%v", descriptors, units, err)
	}
	var mismatched int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM understanding_unit unit
		LEFT JOIN understanding_descriptor descriptor ON descriptor.digest=unit.descriptor_digest
		WHERE descriptor.digest IS NULL`).Scan(&mismatched); err != nil || mismatched != 0 {
		t.Fatalf("units without a descriptor: %d err=%v", mismatched, err)
	}
	// A second open is a no-op, and a new scan reuses the stored descriptors.
	ix.Close()
	if ix, err = Open(path); err != nil {
		t.Fatal(err)
	}
	shapeGeneration(t, ix, "four", 40)
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM understanding_descriptor`).Scan(&descriptors); err != nil || descriptors != 6 {
		t.Fatalf("after a fourth scan descriptors=%d err=%v", descriptors, err)
	}
}

// The edge readers' lookups must be index searches in the new shape: by the primary
// key from the "from" side, by understanding_edge_to from the "to" side.
func TestUnderstandingEdgeLookupsUseAnIndexWithoutRowid(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	for query, want := range map[string]string{
		`SELECT * FROM understanding_edge WHERE generation_id=1 AND from_kind='file' AND from_ref='a.go'`: "USING PRIMARY KEY (generation_id=? AND from_kind=? AND from_ref=?)",
		// The readers' own "to"-side source: without the named index the planner
		// picks the primary key on generation_id alone and reads the whole generation.
		`SELECT * FROM ` + understandingEdgeByTargetSQL + ` WHERE generation_id=1 AND relation='symbol_calls_symbol' AND to_kind='symbol' AND to_ref IN ('go::A','go::B')`:                                                               "USING INDEX understanding_edge_to (generation_id=? AND to_kind=? AND to_ref=?)",
		`SELECT 1 FROM understanding_unit WHERE descriptor_digest='sha256-v1:x'`:                                                                                                                                                         "understanding_unit_descriptor",
		`SELECT d.from_ref FROM understanding_edge d INDEXED BY understanding_edge_to JOIN understanding_generation p ON p.snapshot_digest=d.to_ref WHERE d.generation_id=1 AND d.relation='package_depends_on' AND d.to_kind='package'`: "INDEX understanding_edge_to (generation_id=? AND to_kind=?",
	} {
		rows, err := ix.db.Query(`EXPLAIN QUERY PLAN ` + query)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(detail + "\n")
		}
		rows.Close()
		if !strings.Contains(plan.String(), want) || strings.Contains(plan.String(), "SCAN understanding_edge\n") {
			t.Fatalf("plan for %q:\n%s", query, plan.String())
		}
	}
}

func stubVolumeFree(t *testing.T, bytes int64) {
	t.Helper()
	previous := volumeFreeBytes
	volumeFreeBytes = func(string) (int64, bool) { return bytes, true }
	t.Cleanup(func() { volumeFreeBytes = previous })
}

// A store with rows to copy and no room is refused before anything changes: still
// its earlier schema, still the old layout, still readable by the earlier binary.
func TestV45UpgradeRefusesWithoutRoomAndLeavesTheStoreUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	shapeGeneration(t, ix, "one", 10)
	downgradeUnderstandingShapeToV44(t, ix)
	ix.Close()

	stubVolumeFree(t, 1)
	if _, err := OpenExclusive(path); err == nil || !strings.Contains(err.Error(), "the store is unchanged") {
		t.Fatalf("open without room: %v", err)
	}
	ro, err := openForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	var unitSQL string
	if err := ro.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 44 {
		t.Fatalf("version after a refused migration=%d err=%v", version, err)
	}
	if err := ro.QueryRow(`SELECT sql FROM sqlite_master WHERE name='understanding_unit'`).Scan(&unitSQL); err != nil || !strings.Contains(unitSQL, "descriptor_json") {
		t.Fatalf("unit table after a refused migration: %q err=%v", unitSQL, err)
	}
	ro.Close()

	// With room, the same store migrates. An empty store never needs room.
	// A failure after the rebuild, before the stamp, rolls the whole thing back.
	stubVolumeFree(t, 1<<40)
	migrationTestHook = func(schemaDB) error { return errors.New("injected failure after the rebuild") }
	if _, err := OpenExclusive(path); err == nil {
		t.Fatal("the injected failure did not fail the open")
	}
	migrationTestHook = nil
	if ro, err = openForTest(path); err != nil {
		t.Fatal(err)
	}
	if err := ro.QueryRow(`SELECT sql FROM sqlite_master WHERE name='understanding_unit'`).Scan(&unitSQL); err != nil || !strings.Contains(unitSQL, "descriptor_json") {
		t.Fatalf("unit table after a rolled-back rebuild: %q err=%v", unitSQL, err)
	}
	ro.Close()
	ix = reopenRebuilding(t, path)
	ix.Close()
	stubVolumeFree(t, 1)
	empty, err := Open(filepath.Join(t.TempDir(), "empty.sqlite"))
	if err != nil {
		t.Fatalf("an empty store was refused for room: %v", err)
	}
	empty.Close()
}

func TestUnreferencedUnderstandingDescriptorsAreDeletedInBoundedSteps(t *testing.T) {
	f := newRetentionFixture(t)
	ix := f.ix
	countDescriptors := func() int {
		var n int
		if err := ix.db.QueryRow(`SELECT COUNT(*) FROM understanding_descriptor`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := countDescriptors()
	if more, err := ix.DeleteUnreferencedUnderstandingDescriptors(100); err != nil || more || countDescriptors() != before {
		t.Fatalf("a store with no orphans changed: more=%v err=%v", more, err)
	}
	pruned := 0
	for _, id := range []int64{f.quietBaseline, f.quietCurrent, f.liveIntermediate, f.orphan} {
		if pruneFully(t, ix, id) {
			pruned++
		}
	}
	// Each fixture generation carries its own descriptor text, so four are orphaned.
	steps := 0
	for more := true; more; steps++ {
		var err error
		if more, err = ix.DeleteUnreferencedUnderstandingDescriptors(1); err != nil {
			t.Fatal(err)
		}
	}
	if pruned != 4 || countDescriptors() != before-4 || steps != 5 {
		t.Fatalf("pruned=%d descriptors %d -> %d in %d steps", pruned, before, countDescriptors(), steps)
	}
	var dangling int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM understanding_unit unit WHERE NOT EXISTS(
		SELECT 1 FROM understanding_descriptor descriptor WHERE descriptor.digest=unit.descriptor_digest)`).Scan(&dangling); err != nil || dangling != 0 {
		t.Fatalf("units referencing a deleted descriptor: %d err=%v", dangling, err)
	}
	// A kept generation still reads its descriptor text.
	if units, _, err := ix.UnderstandingUnits(f.liveCurrent, 0, 10); err != nil || len(units) != 1 || !strings.Contains(units[0].DescriptorJSON, "live-current") {
		t.Fatalf("kept generation units=%+v err=%v", units, err)
	}
}

func TestCompactShrinksTheStoreAndRefusesWithoutRoom(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	padding := strings.Repeat("x", 4000)
	for i := 0; i < 300; i++ {
		if _, err := ix.db.Exec(`INSERT INTO understanding_descriptor(digest,descriptor_json) VALUES(?,?)`, fmt.Sprintf("sha256-v1:%04d", i), padding); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ix.db.Exec(`DELETE FROM understanding_descriptor`); err != nil {
		t.Fatal(err)
	}
	ix.Close()

	// A second holder makes the exclusive open fail busy, and the store is untouched.
	holder, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if blocked, err := OpenExclusive(path); err == nil {
		blocked.Close()
		t.Fatal("an exclusive open succeeded beside another open store")
	}
	holder.Close()

	ix, err = OpenExclusive(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	stubVolumeFree(t, 1)
	if _, err := ix.Compact(); err == nil || !strings.Contains(err.Error(), "not compacted") {
		t.Fatalf("compact without room: %v", err)
	}
	stubVolumeFree(t, 1<<40)
	result, err := ix.Compact()
	if err != nil {
		t.Fatal(err)
	}
	info, statErr := os.Stat(path)
	if statErr != nil || result.BytesAfter != info.Size() || result.BytesAfter >= result.BytesBefore || result.BytesBefore-result.BytesAfter < 1<<20 {
		t.Fatalf("compact result %+v, file %d bytes, err=%v", result, info.Size(), statErr)
	}
	if _, _, err := ix.UnderstandingGenerationByID(1); err != nil {
		t.Fatalf("store unreadable after compact: %v", err)
	}
}

// The edge copy reads the old rowid table in primary-key order through its primary-key
// index. A plan that sorted instead would need temporary space the room check does
// not count.
func TestV45EdgeCopyReadsInKeyOrderWithoutASort(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	downgradeUnderstandingShapeToV44(t, ix)
	rows, err := ix.db.Query(`EXPLAIN QUERY PLAN SELECT generation_id,from_kind,from_ref,relation,to_kind,to_ref,source_path,source_line,provenance,analyzer_id,evidence_digest
		FROM understanding_edge
		ORDER BY generation_id,from_kind,from_ref,relation,to_kind,to_ref,source_path,source_line,analyzer_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if !strings.Contains(plan.String(), "USING INDEX sqlite_autoindex_understanding_edge_1") || strings.Contains(plan.String(), "TEMP B-TREE") {
		t.Fatalf("edge copy plan:\n%s", plan.String())
	}
}
