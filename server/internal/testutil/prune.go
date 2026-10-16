package testutil

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PruneOrphanedWorkspaceRows deletes rows in the named workspace-scoped tables
// whose workspace no longer exists, and reports how many rows went away.
//
// Why this exists: a workspace's rows in tables that carry no FK to workspace
// (the no-cascade rule in AGENTS.md) are cleaned by exactly one piece of code —
// the handler's workspace teardown. A test that removes its workspace with a
// direct DELETE instead bypasses that, and what it leaves behind is unreachable
// through the product, because every workspace-scoped query resolves the
// workspace first. Those rows therefore never go away on their own, and the
// shared development and CI databases accumulate them run after run.
//
// Callers pass the tables they consider the workspace's own — internal/handler
// passes the ones its workspace deletion manifest marks as workspaceDelete — so
// this stays a test-hygiene tool and never touches a table the product
// deliberately keeps or detaches (analytics rollups, for instance). A table that
// does have an FK to workspace is skipped: its rows already cascade away, and
// pruning there would mask a cascade that stopped working.
func PruneOrphanedWorkspaceRows(ctx context.Context, pool *pgxpool.Pool, tables []string) (int64, error) {
	var deleted int64
	for _, table := range tables {
		var noForeignKey, hasWorkspaceColumn bool
		err := pool.QueryRow(ctx, `
			SELECT NOT EXISTS (
				       SELECT 1 FROM pg_constraint
				       WHERE contype = 'f'
				         AND conrelid = to_regclass($1)
				         AND confrelid = 'workspace'::regclass
			       ),
			       EXISTS (
				       SELECT 1 FROM information_schema.columns
				       WHERE table_schema = 'public'
				         AND table_name = $2
				         AND column_name = 'workspace_id'
			       )`,
			table, table).Scan(&noForeignKey, &hasWorkspaceColumn)
		if err != nil {
			// to_regclass keeps a renamed table a skip rather than an error, so
			// anything arriving here is a real database problem worth reporting.
			return deleted, fmt.Errorf("inspect workspace table %q: %w", table, err)
		}
		if !noForeignKey || !hasWorkspaceColumn {
			continue
		}
		tag, err := pool.Exec(ctx, fmt.Sprintf(
			"DELETE FROM %s WHERE workspace_id NOT IN (SELECT id FROM workspace)",
			quoteIdent(table)))
		if err != nil {
			return deleted, fmt.Errorf("prune orphaned rows in %q: %w", table, err)
		}
		deleted += tag.RowsAffected()
	}
	return deleted, nil
}
