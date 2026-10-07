package store

import "fmt"

// migrateTeamRestOfReleaseV46 is schema 46 (team rest-of-release plan §5.3, §6.7):
// named model routes referenced from both binding tables, the adoption key a place
// records, and the device's handoff state. Every step is additive and probes the
// layout, so it runs once and is correct on a fresh store. Each area's DDL lives in the
// file of the area that owns its rows.
func migrateTeamRestOfReleaseV46(db schemaDB) error {
	if err := migrateBindingRoutesV46(db); err != nil {
		return err
	}
	if err := migrateHandoffV46(db); err != nil {
		return err
	}
	return migrateHandoffOpenV46(db)
}

// addColumnsV46 adds each missing column to a table that exists. A table that does not
// exist yet is created with its full layout by its owner later in the same open.
func addColumnsV46(db schemaDB, table string, additions []struct{ name, ddl string }) error {
	cols, err := columnSet(db, table)
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return nil
	}
	for _, addition := range additions {
		if cols[addition.name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + addition.ddl); err != nil {
			return fmt.Errorf("migrate v46: %s.%s: %w", table, addition.name, err)
		}
	}
	return nil
}
