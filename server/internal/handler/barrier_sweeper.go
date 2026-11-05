package handler

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// barrierSweepIdle is how long a gated issue must stay quiet before the
// sweeper advances it. It exists so a run that is still wrapping up (its task
// terminal, its last comments being flushed) is not advanced underneath itself.
const barrierSweepIdle = 10 * time.Minute

// barrierReadyIssuesSQL finds issues whose acceptance gate was posted but
// whose status never reached `done`.
//
// The structural problem it fixes is generic, not tied to one workflow:
//
//   - A child completes, posts `✅ 验收门`, moves to `in_review`, and stops.
//     Nothing advances it, because the only automatic path into `done` used to
//     be a human or the periodic acceptance sampler.
//   - The parent's `--stage` barrier (issue_child_done.go) closes only when
//     every child of the stage is TERMINAL (`done`/`cancelled`). An `in_review`
//     child therefore holds the whole stage open, and the parent assignee is
//     never woken to aggregate.
//
// A gated, quiet, idle issue is by definition ready: the work has been
// verified, no run is in flight, and nobody has asked for changes. The sweeper
// advances it to `done` so the barrier can close. Acceptance sampling stays a
// post-hoc audit: a sampler that finds expired evidence reopens the issue,
// which is exactly why this sweep may be aggressive.
//
// Guards are structural: the gate comment must exist, the issue must be
// in_progress/in_review, no task may be queued/dispatched/running/waiting, the
// last comment must not be a wait (`⏳`) or approval (`⛔`) marker, and a parent
// must have no unfinished child.
const barrierReadyIssuesSQL = `
WITH gate AS (
    SELECT issue_id, MAX(created_at) AS gate_at
    FROM comment
    WHERE deleted_at IS NULL AND content LIKE '✅ 验收门%'
    GROUP BY issue_id
), lastc AS (
    SELECT DISTINCT ON (issue_id) issue_id, content, created_at
    FROM comment
    WHERE deleted_at IS NULL
    ORDER BY issue_id, created_at DESC
), active AS (
    SELECT DISTINCT issue_id
    FROM agent_task_queue
    WHERE status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
), kids AS (
    SELECT parent_issue_id,
           COUNT(*) FILTER (WHERE status NOT IN ('done', 'cancelled')) AS unfinished
    FROM issue
    WHERE parent_issue_id IS NOT NULL
    GROUP BY parent_issue_id
)
SELECT i.id::text
FROM issue i
JOIN gate g ON g.issue_id = i.id
JOIN lastc l ON l.issue_id = i.id
LEFT JOIN active a ON a.issue_id = i.id
LEFT JOIN kids k ON k.parent_issue_id = i.id
WHERE i.status IN ('in_progress', 'in_review')
  AND a.issue_id IS NULL
  AND l.created_at < now() - make_interval(secs => $1)
  AND l.content NOT LIKE '⏳%'
  AND l.content NOT LIKE '⛔%'
  AND COALESCE(k.unfinished, 0) = 0
ORDER BY l.created_at
LIMIT 100
`

// SweepBarrierLiveness advances every gated, idle issue to `done` and lets the
// normal child-done notification close the stage barrier. It returns the number
// of issues advanced. Called from a server-side ticker; best-effort per row so
// one bad row cannot stall the rest.
func (h *Handler) SweepBarrierLiveness(ctx context.Context) int {
	rows, err := h.DB.Query(ctx, barrierReadyIssuesSQL, int(barrierSweepIdle.Seconds()))
	if err != nil {
		slog.Warn("barrier sweeper: candidate query failed", "error", err)
		return 0
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			slog.Warn("barrier sweeper: scan failed", "error", err)
			continue
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		slog.Warn("barrier sweeper: rows iteration failed", "error", err)
	}

	moved := 0
	for _, raw := range ids {
		id, err := util.ParseUUID(raw)
		if err != nil {
			continue
		}
		prev, err := h.Queries.GetIssue(ctx, id)
		if err != nil {
			slog.Warn("barrier sweeper: load issue failed", "issue_id", raw, "error", err)
			continue
		}
		if prev.Status != "in_progress" && prev.Status != "in_review" {
			continue
		}
		updated, err := h.Queries.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{
			ID:          prev.ID,
			WorkspaceID: prev.WorkspaceID,
			Status:      "done",
		})
		if err != nil {
			slog.Warn("barrier sweeper: advance failed", "issue_id", raw, "error", err)
			continue
		}
		if _, err := h.Queries.CreateComment(ctx, db.CreateCommentParams{
			ID:          dbid.NewV7(),
			IssueID:     prev.ID,
			WorkspaceID: prev.WorkspaceID,
			AuthorType:  "system",
			AuthorID:    pgtype.UUID{Bytes: [16]byte{}, Valid: true},
			Content:     "验收门已通过，且无运行任务、静止超过 10 分钟，自动置为 `done`（barrier sweeper）。需要返工时由抽检或负责人重新打开。",
			Type:        "comment",
		}); err != nil {
			slog.Warn("barrier sweeper: comment failed", "issue_id", raw, "error", err)
		}
		// Closes the parent's stage barrier when this was the last open child.
		h.notifyParentOfChildDone(ctx, prev, updated)
		moved++
	}
	if moved > 0 {
		slog.Info("barrier sweeper: advanced gated issues", "count", moved)
	}
	return moved
}
