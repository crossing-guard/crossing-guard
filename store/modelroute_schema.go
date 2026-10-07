package store

// bindingRouteColumnsV46 are the columns schema 46 adds to BOTH binding tables (plan
// §5.3, §4.1 decision 5). route_id and route_revision_digest reference a named model
// route's current revision; the existing runtime/model/effort (managed) and
// endpoint/model (review) columns stay and become the resolved copy of that revision.
// route_problem is ” or one of the typed problems a start-up pass or a failed
// migration sets. adoption_key is ” unless the place was turned on for a revision whose
// selection a team adoption wrote; it is "<organization id>\x1f<scope key>".
var bindingRouteColumnsV46 = []struct{ name, ddl string }{
	{"route_id", `route_id TEXT NOT NULL DEFAULT ''`},
	{"route_revision_digest", `route_revision_digest TEXT NOT NULL DEFAULT ''`},
	{"route_problem", `route_problem TEXT NOT NULL DEFAULT '' CHECK(route_problem IN ('','route_missing','migration_failed'))`},
	{"adoption_key", `adoption_key TEXT NOT NULL DEFAULT ''`},
}

// Route problems a binding can carry.
const (
	RouteProblemMissing         = "route_missing"
	RouteProblemMigrationFailed = "migration_failed"
)

func migrateBindingRoutesV46(db schemaDB) error {
	for _, table := range []string{"orchestration_managed_binding", "orchestration_review_binding"} {
		if err := addColumnsV46(db, table, bindingRouteColumnsV46); err != nil {
			return err
		}
	}
	return nil
}
