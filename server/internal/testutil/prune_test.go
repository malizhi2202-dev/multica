package testutil

import (
	"context"
	"testing"
)

// PruneOrphanedWorkspaceRows runs against the shared development database, where the
// rows it deletes belong to nobody. That combination is worth pinning down: it must
// delete exactly the unreachable set, leave anything a live workspace can still see,
// and treat a table it cannot clean as a skip rather than an error.

func TestPruneRemovesOnlyRowsWhoseWorkspaceIsGone(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	const slug = "testutil-prune-orphan"
	if _, err := testPool.Exec(ctx, `DELETE FROM workspace WHERE slug = $1`, slug); err != nil {
		t.Fatalf("clear an earlier run: %v", err)
	}
	var goneID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO workspace (name, slug, issue_prefix) VALUES ('Prune Target', $1, 'PRN') RETURNING id`,
		slug).Scan(&goneID); err != nil {
		t.Fatalf("create the workspace that will disappear: %v", err)
	}

	insertStatus := func(workspaceID, key string) string {
		t.Helper()
		var id string
		err := testPool.QueryRow(ctx, `
			INSERT INTO issue_status (workspace_id, key, name, category, color)
			VALUES ($1, $2, $2, 'unstarted', '#123456')
			RETURNING id`, workspaceID, key).Scan(&id)
		if err != nil {
			t.Fatalf("insert status %q: %v", key, err)
		}
		return id
	}
	doomed := insertStatus(goneID, "prune_doomed")
	// A row on the suite's own surviving workspace: the one thing this prune must
	// never touch.
	survivor := insertStatus(testWorkspaceID, "prune_survivor")
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue_status WHERE id = $1`, survivor)
	})

	// member has an FK to workspace, so workspace teardown already cascades it: the
	// helper has to leave that table alone without complaining. An unknown name must
	// be skipped too, since the caller's list comes from a manifest that can lag a
	// rename by one commit.
	if _, err := testPool.Exec(ctx,
		`INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`,
		goneID, testUserID); err != nil {
		t.Fatalf("insert a member row for the doomed workspace: %v", err)
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM workspace WHERE id = $1`, goneID); err != nil {
		t.Fatalf("delete the workspace row: %v", err)
	}

	deleted, err := PruneOrphanedWorkspaceRows(ctx, testPool,
		[]string{"issue_status", "member", "this_table_does_not_exist"})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if deleted != 1 {
		t.Errorf("prune deleted %d rows, want 1 (the orphaned status only)", deleted)
	}
	if statusExists(t, ctx, doomed) {
		t.Error("the orphaned status row survived the prune")
	}
	if !statusExists(t, ctx, survivor) {
		t.Error("the prune deleted a row a live workspace can still see")
	}
}

func statusExists(t *testing.T, ctx context.Context, id string) bool {
	t.Helper()
	var found bool
	if err := testPool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM issue_status WHERE id = $1)`, id).Scan(&found); err != nil {
		t.Fatalf("check status row %s: %v", id, err)
	}
	return found
}
