package main

import (
	"database/sql"
	"fmt"
	"strings"
)

// V4 extends only the event CHECK constraints. Existing proposal rows and event
// identities, ordinals, timestamps and categories are copied without alteration.
// Keeping the lifecycle state executing preserves compatibility and denies any
// second dispatch; the new event records why execution is no longer confirmed.
func migrateMiniAIActionSchemaV4(tx *sql.Tx) error {
	eventsDDL := strings.Replace(actionSchemaV3TableStatements[1],
		"'execution_succeeded','execution_failed'", "'execution_succeeded','execution_failed','execution_outcome_unknown'", 1)
	eventsDDL = strings.Replace(eventsDDL,
		"OR (event_type = 'execution_failed'", `OR (event_type = 'execution_outcome_unknown'
				AND from_state IS NOT NULL
				AND from_state = 'executing' AND to_state = 'executing'
				AND failure_category IS NOT NULL)
			OR (event_type = 'execution_failed'`, 1)
	for _, statement := range []string{
		`DROP INDEX action_events_action_ordinal_idx`,
		`DROP INDEX action_events_action_type_idx`,
		`ALTER TABLE action_events RENAME TO action_events_v3`,
		eventsDDL,
		`INSERT INTO action_events SELECT * FROM action_events_v3`,
		`DROP TABLE action_events_v3`,
		actionSchemaV3IndexStatements[1],
		actionSchemaV3IndexStatements[2],
	} {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("migrate action audit schema version 4: %w", err)
		}
	}
	return nil
}
