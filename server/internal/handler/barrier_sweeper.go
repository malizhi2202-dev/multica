package handler

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
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
  -- A wait/approval marker blocks auto-advance only while the work is still
  -- in_progress. Once the issue is in_review, a later ⏳/⛔ is a response in
  -- the thread (e.g. a reviewer declining to act, or a clarification), not
  -- outstanding work: the gate comment already recorded acceptance.
  AND (i.status = 'in_review' OR (l.content NOT LIKE '⏳%' AND l.content NOT LIKE '⛔%'))
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

	// Backfill: children that reached done before the handoff existed (or while
	// the sweeper was down) still owe their parent a result summary. Deduped by
	// the per-child marker embedded in the comment body.
	h.backfillChildResultHandoffs(ctx)

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
		var gate string
		_ = h.DB.QueryRow(ctx,
			`SELECT content FROM comment WHERE issue_id = $1 AND deleted_at IS NULL AND content LIKE '✅ 验收门%' ORDER BY created_at DESC LIMIT 1`,
			prev.ID).Scan(&gate)
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
		// Return the result to the parent: the parent must be able to aggregate
		// a child's acceptance without re-reading the child issue.
		h.postChildResultToParent(ctx, prev, gate)
		// Closes the parent's stage barrier when this was the last open child.
		h.notifyParentOfChildDone(ctx, prev, updated)
		moved++
	}
	if moved > 0 {
		slog.Info("barrier sweeper: advanced gated issues", "count", moved)
	}
	return moved
}

// postChildResultToParent hands a completed child's acceptance summary back to
// its parent issue, so the parent can synthesize results incrementally instead
// of waiting for the whole stage barrier and then re-reading every child.
//
// The comment is authored by system (zero UUID) and mentions the parent's
// assignee when it is an agent or a squad, which is what makes the handoff
// visible in the parent's own thread. It deliberately does NOT enqueue a run:
// the stage barrier still decides when the parent is woken, so a wide stage
// does not turn into one parent run per child.
func (h *Handler) postChildResultToParent(ctx context.Context, child db.Issue, gate string) {
	if !child.ParentIssueID.Valid {
		return
	}
	parent, err := h.Queries.GetIssue(ctx, child.ParentIssueID)
	if err != nil {
		return
	}
	switch parent.Status {
	case "done", "cancelled", "backlog":
		return
	}

	mention := ""
	if parent.AssigneeType.Valid && parent.AssigneeID.Valid {
		switch parent.AssigneeType.String {
		case "agent":
			if ag, err := h.Queries.GetAgent(ctx, parent.AssigneeID); err == nil {
				mention = fmt.Sprintf("[@%s](mention://agent/%s) ", ag.Name, uuidToString(parent.AssigneeID))
			}
		case "squad":
			if sq, err := h.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{
				ID:          parent.AssigneeID,
				WorkspaceID: parent.WorkspaceID,
			}); err == nil {
				mention = fmt.Sprintf("[@%s](mention://squad/%s) ", sq.Name, uuidToString(parent.AssigneeID))
			}
		}
	}

	summary := strings.TrimSpace(gate)
	if summary == "" {
		summary = "（子任务未留验收摘要）"
	}
	if runes := []rune(summary); len(runes) > 300 {
		summary = string(runes[:300]) + "…"
	}
	content := fmt.Sprintf(
		"%s子任务结果回传：[#%d %s](mention://issue/%s) 已验收通过并置 done。\n验收摘要：%s\n完整证据见子 issue；请汇总后推进父级。\n<!-- handoff:%s -->",
		mention, child.Number, child.Title, uuidToString(child.ID), summary, uuidToString(child.ID),
	)
	if _, err := h.Queries.CreateComment(ctx, db.CreateCommentParams{
		ID:          dbid.NewV7(),
		IssueID:     parent.ID,
		WorkspaceID: parent.WorkspaceID,
		AuthorType:  "system",
		AuthorID:    pgtype.UUID{Bytes: [16]byte{}, Valid: true},
		Content:     content,
		Type:        "comment",
	}); err != nil {
		slog.Warn("barrier sweeper: parent handoff failed", "parent_id", uuidToString(parent.ID), "error", err)
	}
}

// backfillDoneChildrenHandoffsSQL finds done children whose parent is still
// active and that never received this child's handoff comment.
const backfillDoneChildrenHandoffsSQL = `
SELECT c.id::text
FROM issue c
JOIN issue p ON p.id = c.parent_issue_id
JOIN LATERAL (
    SELECT 1 FROM comment g
    WHERE g.issue_id = c.id AND g.deleted_at IS NULL AND g.content LIKE '✅ 验收门%'
    LIMIT 1
) gate ON true
WHERE c.status = 'done'
  AND p.status IN ('in_progress', 'in_review')
  AND NOT EXISTS (
    SELECT 1 FROM comment h
    WHERE h.issue_id = p.id AND h.deleted_at IS NULL
      AND h.content LIKE '%handoff:' || c.id::text || ' -->%'
  )
ORDER BY c.updated_at DESC
LIMIT 20
`

// backfillChildResultHandoffs posts the handoff for children that finished
// before the parent handoff existed. It is idempotent via the embedded marker,
// so a restart or a repeated tick never duplicates a summary.
func (h *Handler) backfillChildResultHandoffs(ctx context.Context) {
	rows, err := h.DB.Query(ctx, backfillDoneChildrenHandoffsSQL)
	if err != nil {
		slog.Warn("barrier sweeper: handoff backfill query failed", "error", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	rows.Close()

	posted := 0
	for _, raw := range ids {
		id, err := util.ParseUUID(raw)
		if err != nil {
			continue
		}
		child, err := h.Queries.GetIssue(ctx, id)
		if err != nil {
			continue
		}
		var gate string
		_ = h.DB.QueryRow(ctx,
			`SELECT content FROM comment WHERE issue_id = $1 AND deleted_at IS NULL AND content LIKE '✅ 验收门%' ORDER BY created_at DESC LIMIT 1`,
			child.ID).Scan(&gate)
		h.postChildResultToParent(ctx, child, gate)
		posted++
	}
	if posted > 0 {
		slog.Info("barrier sweeper: backfilled child result handoffs", "count", posted)
	}
}
