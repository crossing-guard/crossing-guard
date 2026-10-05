package store

// Understanding storage shape (schema 45, understanding-facts-retention plan §5):
// content-addressed unit descriptors, the edge table without a rowid, the one-time
// rebuild that moves an existing store to that shape, and Compact, which returns the
// space the rebuild and retention freed.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
)

// ErrUnderstandingRebuildPending is what an ordinary Open returns for a store that
// still has unit or edge rows in the pre-45 layout. The rebuild rewrites both
// tables, so it runs only under an exclusive open: the daemon performs it when it
// starts, and crossing-guard compact performs it with the daemon stopped.
var ErrUnderstandingRebuildPending = errors.New("the store's understanding tables need the one-time schema-45 rebuild; " +
	"the daemon performs it when it starts, or stop the daemon and run `crossing-guard compact`")

// OpenAsOwner opens the store for the daemon that owns it. A store still holding rows
// in the pre-45 understanding layout is rebuilt here, once, under an exclusive
// open, then reopened normally: the daemon is the one process entitled to do that at
// start, and refusing would leave it running with no governor after any reload. The
// rebuild is atomic and is refused, store untouched, when its volume lacks room.
func OpenAsOwner(path string) (*Index, error) {
	ix, err := Open(path)
	if !errors.Is(err, ErrUnderstandingRebuildPending) {
		return ix, err
	}
	log.Printf("store: rebuilding the understanding tables for schema %d before serving; this runs once and can take minutes", SchemaVersion)
	started := time.Now()
	exclusive, err := OpenExclusive(path)
	if err != nil {
		return nil, fmt.Errorf("schema-%d rebuild: %w", SchemaVersion, err)
	}
	if err := exclusive.Close(); err != nil {
		return nil, fmt.Errorf("schema-%d rebuild: close: %w", SchemaVersion, err)
	}
	log.Printf("store: understanding tables rebuilt in %s", time.Since(started).Round(time.Second))
	return Open(path)
}

// understandingShapeRebuildPending reports whether the store holds rows the schema-45
// rebuild would copy. A store without the tables, or with empty ones, has nothing to
// copy and any open may migrate it.
func understandingShapeRebuildPending(db schemaDB) (bool, error) {
	rebuildUnits, rebuildEdges, err := understandingShapeProbe(db)
	if err != nil || (!rebuildUnits && !rebuildEdges) {
		return false, err
	}
	var rows bool
	err = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM understanding_unit) OR EXISTS(SELECT 1 FROM understanding_edge)`).Scan(&rows)
	return rows, err
}

// understandingShapeProbe reads the layout: whether the unit table still carries
// descriptor text inline, and whether the edge table still has a rowid.
func understandingShapeProbe(db schemaDB) (rebuildUnits, rebuildEdges bool, err error) {
	unitColumns, err := columnSet(db, "understanding_unit")
	if err != nil {
		return false, false, err
	}
	var edgeSQL string
	if err := db.QueryRow(`SELECT COALESCE(sql,'') FROM sqlite_master WHERE type='table' AND name='understanding_edge'`).Scan(&edgeSQL); err != nil && err != sql.ErrNoRows {
		return false, false, fmt.Errorf("inspect understanding edge table: %w", err)
	}
	return unitColumns["descriptor_json"], edgeSQL != "" && !strings.Contains(edgeSQL, "WITHOUT ROWID"), nil
}

// StoredSchemaVersion reads a store file's schema stamp without opening it as a
// store: no DDL, no migration, read-only.
func StoredSchemaVersion(path string) (int, error) {
	db, err := driver.Open("file:"+path+"?mode=ro&_pragma=busy_timeout(500)", fts5.Register)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var version int
	err = db.QueryRow(`PRAGMA user_version`).Scan(&version)
	return version, err
}

// UnderstandingDescriptorDigest names one descriptor text. Two units share a stored
// descriptor exactly when their texts are byte-identical.
func UnderstandingDescriptorDigest(descriptorJSON string) string {
	sum := sha256.Sum256([]byte(descriptorJSON))
	return "sha256-v1:" + hex.EncodeToString(sum[:])
}

const understandingUnitTableV45 = `CREATE TABLE understanding_unit(
  generation_id INTEGER NOT NULL REFERENCES understanding_generation(id) ON DELETE RESTRICT,
  path TEXT NOT NULL,
  source_hash TEXT NOT NULL CHECK(source_hash LIKE 'sha256-v1:%'),
  language TEXT NOT NULL DEFAULT '',
  namespace TEXT NOT NULL DEFAULT '',
  descriptor_digest TEXT NOT NULL REFERENCES understanding_descriptor(digest) ON DELETE RESTRICT,
  PRIMARY KEY(generation_id,path)
)`

const understandingEdgeTableV45 = `CREATE TABLE understanding_edge(
  generation_id INTEGER NOT NULL REFERENCES understanding_generation(id) ON DELETE RESTRICT,
  from_kind TEXT NOT NULL CHECK(from_kind IN ('file','package','symbol','document','item')),
  from_ref TEXT NOT NULL,
  relation TEXT NOT NULL CHECK(relation IN (
    'file_in_package','package_depends_on','file_declares_symbol',
    'symbol_calls_symbol','file_references_symbol',
    'document_references_file','document_defines_item','item_references_file'
  )),
  to_kind TEXT NOT NULL CHECK(to_kind IN ('file','package','symbol','document','item')),
  to_ref TEXT NOT NULL,
  source_path TEXT NOT NULL DEFAULT '',
  source_line INTEGER NOT NULL DEFAULT 0 CHECK(source_line >= 0),
  provenance TEXT NOT NULL CHECK(provenance='measured'),
  analyzer_id TEXT NOT NULL,
  evidence_digest TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(generation_id,from_kind,from_ref,relation,to_kind,to_ref,source_path,source_line,analyzer_id),
  CHECK(
    (relation='file_in_package' AND from_kind='file' AND to_kind='package') OR
    (relation='package_depends_on' AND from_kind='package' AND to_kind='package') OR
    (relation='file_declares_symbol' AND from_kind='file' AND to_kind='symbol') OR
    (relation='symbol_calls_symbol' AND from_kind='symbol' AND to_kind='symbol') OR
    (relation='file_references_symbol' AND from_kind='file' AND to_kind='symbol') OR
    (relation='document_references_file' AND from_kind='document' AND to_kind='file') OR
    (relation='document_defines_item' AND from_kind='document' AND to_kind='item') OR
    (relation='item_references_file' AND from_kind='item' AND to_kind='file')
  )
) WITHOUT ROWID`

// understandingShapeBatch is how many unit rows the descriptor rebuild reads before it
// writes them: the rows are read and closed first, so no cursor is open across a write.
// A variable so a test can force the copy across many batch boundaries.
var understandingShapeBatch = 500

// volumeFreeBytes reports the bytes free on the volume holding a directory. A test
// replaces it; on a platform that cannot answer it reports false and no check is made.
var volumeFreeBytes = platformVolumeFreeBytes

// migrateUnderstandingShapeV45 rebuilds the unit and edge tables into the schema-45
// shape. Each half probes the LAYOUT, not the version: a fresh store already has the
// new shape from the base DDL and has nothing to copy. The copy is proportional to
// the rows present, and both halves run inside the store's one open transaction, so
// the step first refuses a store whose volume cannot hold the rebuild.
func migrateUnderstandingShapeV45(db schemaDB) error {
	rebuildUnits, rebuildEdges, err := understandingShapeProbe(db)
	if err != nil {
		return fmt.Errorf("migrate understanding shape v45: %w", err)
	}
	if rebuildUnits || rebuildEdges {
		if err := refuseUnderstandingRebuildWithoutRoom(db); err != nil {
			return err
		}
	}
	if rebuildUnits {
		if err := rebuildUnderstandingUnitsV45(db); err != nil {
			return err
		}
	}
	if rebuildEdges {
		if err := rebuildUnderstandingEdgesV45(db); err != nil {
			return err
		}
	}
	// Every store: the index that serves the descriptor foreign key and the orphan
	// delete, and the retired "from" index, which a store older than v18 recreates on
	// its way here.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS understanding_unit_descriptor ON understanding_unit(descriptor_digest);
DROP INDEX IF EXISTS understanding_edge_from`); err != nil {
		return fmt.Errorf("migrate understanding shape v45: indexes: %w", err)
	}
	return nil
}

// storeSpace is the store's size in bytes: the whole file and the part of it that is
// free pages.
type storeSpace struct {
	path string
	live int64
	free int64
}

func readStoreSpace(db schemaDB) (storeSpace, error) {
	var space storeSpace
	var pages, freePages, pageSize int64
	if err := db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&space.path); err != nil {
		return space, err
	}
	if err := db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		return space, err
	}
	if err := db.QueryRow(`PRAGMA freelist_count`).Scan(&freePages); err != nil {
		return space, err
	}
	if err := db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return space, err
	}
	space.live, space.free = (pages-freePages)*pageSize, freePages*pageSize
	return space, nil
}

// refuseUnderstandingRebuildWithoutRoom bounds the rebuild from above. The new tables
// are written to the write-ahead log first and the old tables' pages are freed only
// at commit, so with live size L and free-list bytes F the step can need L for the
// log plus max(0, L-F) of file growth. L over-estimates the two rebuilt tables on
// purpose. A store with nothing to copy needs no room.
func refuseUnderstandingRebuildWithoutRoom(db schemaDB) error {
	var rows bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM understanding_unit) OR EXISTS(SELECT 1 FROM understanding_edge)`).Scan(&rows); err != nil {
		return fmt.Errorf("migrate understanding shape v45: probe rows: %w", err)
	}
	if !rows {
		return nil
	}
	space, err := readStoreSpace(db)
	if err != nil {
		return fmt.Errorf("migrate understanding shape v45: measure store: %w", err)
	}
	if space.path == "" {
		return nil
	}
	need := space.live
	if growth := space.live - space.free; growth > 0 {
		need += growth
	}
	available, known := volumeFreeBytes(filepath.Dir(space.path))
	if !known {
		log.Printf("store: free space on the store's volume is unknown; the schema-45 rebuild proceeds without a room check (it can need %d bytes)", need)
		return nil
	}
	if available < need {
		return fmt.Errorf("migrate understanding shape v45: the rebuild can need %d bytes free on the store's volume and %d are free; "+
			"the store is unchanged at its previous schema. Free space; or run the previous binary until understanding "+
			"retention has drained, which shrinks what must be copied — this binary cannot open the store to do that", need, available)
	}
	return nil
}

func rebuildUnderstandingUnitsV45(db schemaDB) error {
	if _, err := db.Exec(`DROP TRIGGER IF EXISTS understanding_unit_complete;
ALTER TABLE understanding_unit RENAME TO understanding_unit_v44;
` + understandingUnitTableV45); err != nil {
		return fmt.Errorf("migrate understanding units v45: create: %w", err)
	}
	stored := map[string]bool{}
	afterGeneration, afterPath := int64(-1), ""
	for {
		batch, err := readUnderstandingUnitsBeforeV45(db, afterGeneration, afterPath)
		if err != nil {
			return fmt.Errorf("migrate understanding units v45: read: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, unit := range batch {
			digest := UnderstandingDescriptorDigest(unit.descriptorJSON)
			if !stored[digest] {
				if _, err := db.Exec(`INSERT OR IGNORE INTO understanding_descriptor(digest,descriptor_json) VALUES(?,?)`, digest, unit.descriptorJSON); err != nil {
					return fmt.Errorf("migrate understanding units v45: descriptor: %w", err)
				}
				stored[digest] = true
			}
			if _, err := db.Exec(`INSERT INTO understanding_unit(generation_id,path,source_hash,language,namespace,descriptor_digest) VALUES(?,?,?,?,?,?)`,
				unit.generationID, unit.path, unit.sourceHash, unit.language, unit.namespace, digest); err != nil {
				return fmt.Errorf("migrate understanding units v45: unit: %w", err)
			}
		}
		last := batch[len(batch)-1]
		afterGeneration, afterPath = last.generationID, last.path
	}
	if err := requireSameRowCount(db, "understanding_unit_v44", "understanding_unit"); err != nil {
		return fmt.Errorf("migrate understanding units v45: %w", err)
	}
	if _, err := db.Exec(`DROP TABLE understanding_unit_v44;
CREATE TRIGGER understanding_unit_complete BEFORE INSERT ON understanding_unit
BEGIN
  SELECT CASE WHEN COALESCE((SELECT status FROM understanding_generation WHERE id=NEW.generation_id),'')!='complete'
    THEN RAISE(ABORT,'understanding units require a complete generation') END;
END;`); err != nil {
		return fmt.Errorf("migrate understanding units v45: finish: %w", err)
	}
	return nil
}

type understandingUnitBeforeV45 struct {
	generationID                                          int64
	path, sourceHash, language, namespace, descriptorJSON string
}

// readUnderstandingUnitsBeforeV45 reads the next batch in primary-key order and closes the
// cursor before returning.
func readUnderstandingUnitsBeforeV45(db schemaDB, afterGeneration int64, afterPath string) ([]understandingUnitBeforeV45, error) {
	rows, err := db.Query(`SELECT generation_id,path,source_hash,language,namespace,descriptor_json
		FROM understanding_unit_v44 WHERE (generation_id,path)>(?,?)
		ORDER BY generation_id,path LIMIT ?`, afterGeneration, afterPath, understandingShapeBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]understandingUnitBeforeV45, 0, understandingShapeBatch)
	for rows.Next() {
		var unit understandingUnitBeforeV45
		if err := rows.Scan(&unit.generationID, &unit.path, &unit.sourceHash, &unit.language, &unit.namespace, &unit.descriptorJSON); err != nil {
			return nil, err
		}
		out = append(out, unit)
	}
	return out, rows.Err()
}

// rebuildUnderstandingEdgesV45 copies in primary-key order, so the new table is
// written as one ascending run, and builds the one secondary index after the copy.
func rebuildUnderstandingEdgesV45(db schemaDB) error {
	if _, err := db.Exec(`DROP TRIGGER IF EXISTS understanding_edge_complete;
DROP INDEX IF EXISTS understanding_edge_from;
DROP INDEX IF EXISTS understanding_edge_to;
ALTER TABLE understanding_edge RENAME TO understanding_edge_v44;
` + understandingEdgeTableV45 + `;
INSERT INTO understanding_edge(generation_id,from_kind,from_ref,relation,to_kind,to_ref,source_path,source_line,provenance,analyzer_id,evidence_digest)
  SELECT generation_id,from_kind,from_ref,relation,to_kind,to_ref,source_path,source_line,provenance,analyzer_id,evidence_digest
  FROM understanding_edge_v44
  ORDER BY generation_id,from_kind,from_ref,relation,to_kind,to_ref,source_path,source_line,analyzer_id`); err != nil {
		return fmt.Errorf("migrate understanding edges v45: copy: %w", err)
	}
	if err := requireSameRowCount(db, "understanding_edge_v44", "understanding_edge"); err != nil {
		return fmt.Errorf("migrate understanding edges v45: %w", err)
	}
	if _, err := db.Exec(`DROP TABLE understanding_edge_v44;
CREATE INDEX understanding_edge_to ON understanding_edge(generation_id,to_kind,to_ref);
CREATE TRIGGER understanding_edge_complete BEFORE INSERT ON understanding_edge
BEGIN
  SELECT CASE WHEN COALESCE((SELECT status FROM understanding_generation WHERE id=NEW.generation_id),'')!='complete'
    THEN RAISE(ABORT,'understanding edges require a complete generation') END;
END;`); err != nil {
		return fmt.Errorf("migrate understanding edges v45: %w", err)
	}
	return nil
}

// requireSameRowCount refuses to let a rebuild drop its source table unless the copy
// holds exactly as many rows. The whole migration is one transaction, so the error
// leaves the store as it was.
func requireSameRowCount(db schemaDB, source, copy string) error {
	var sourceRows, copyRows int64
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM `+source+`),(SELECT COUNT(*) FROM `+copy+`)`).Scan(&sourceRows, &copyRows); err != nil {
		return err
	}
	if sourceRows != copyRows {
		return fmt.Errorf("copied %d of %d rows from %s; refusing to drop it", copyRows, sourceRows, source)
	}
	return nil
}

// DeleteUnreferencedUnderstandingDescriptors deletes up to maxRows descriptors that
// no unit references, in one write transaction, and reports whether more remain.
// Retention calls it after a pass that deleted unit rows.
func (ix *Index) DeleteUnreferencedUnderstandingDescriptors(maxRows int) (bool, error) {
	if maxRows <= 0 {
		return false, fmt.Errorf("understanding descriptor delete needs a positive row budget")
	}
	result, err := ix.db.Exec(`DELETE FROM understanding_descriptor WHERE digest IN (
		SELECT descriptor.digest FROM understanding_descriptor descriptor
		WHERE NOT EXISTS(SELECT 1 FROM understanding_unit unit WHERE unit.descriptor_digest=descriptor.digest)
		LIMIT ?)`, maxRows)
	if err != nil {
		return false, err
	}
	deleted, err := result.RowsAffected()
	return deleted == int64(maxRows), err
}

// CompactResult is what one Compact did to the store file.
type CompactResult struct {
	BytesBefore int64
	BytesAfter  int64
}

// Compact rewrites the store file without its free pages. It is the store's other
// whole-file operation beside Export, and the only way a store that retention has
// emptied gets smaller on disk. Call it on an exclusively opened store
// (OpenExclusive); with another connection present the log truncation reports busy
// and Compact stops. It truncates the write-ahead log first, so a migration's log
// does not count against the room check, then refuses when the volume has less than
// twice the live size free — VACUUM writes a complete copy and logs it. A refusal
// leaves the file's contents as they were; it does not undo a migration the open
// already committed.
func (ix *Index) Compact() (CompactResult, error) {
	if err := ix.truncateWriteAheadLog(); err != nil {
		return CompactResult{}, err
	}
	space, err := readStoreSpace(ix.db)
	if err != nil {
		return CompactResult{}, fmt.Errorf("compact: measure store: %w", err)
	}
	result := CompactResult{BytesBefore: space.live + space.free}
	if available, known := volumeFreeBytes(filepath.Dir(space.path)); known && available < 2*space.live {
		return result, fmt.Errorf("compact: VACUUM can need %d bytes free on the store's volume and %d are free; not compacted",
			2*space.live, available)
	}
	if _, err := ix.db.Exec(`VACUUM`); err != nil {
		return result, fmt.Errorf("compact: %w", err)
	}
	if err := ix.truncateWriteAheadLog(); err != nil {
		return result, err
	}
	info, err := os.Stat(space.path)
	if err != nil {
		return result, err
	}
	result.BytesAfter = info.Size()
	return result, nil
}

// truncateWriteAheadLog checkpoints and empties the log. The pragma answers a busy
// flag in its result row rather than an error, so the row is read.
func (ix *Index) truncateWriteAheadLog() error {
	var busy, logFrames, checkpointed int
	if err := ix.db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("compact: truncate the write-ahead log: %w", err)
	}
	if busy != 0 {
		return fmt.Errorf("compact: the write-ahead log is busy: another connection holds the store; not compacted")
	}
	return nil
}
