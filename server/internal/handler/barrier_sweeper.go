package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
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
// last comment must not be a wait (`⏳`) or approval (`⛔`) marker, no agent or
// member may have spoken AFTER the newest agent-authored gate comment, and a
// parent must have no unfinished child.
const barrierReadyIssuesSQL = `
WITH gate_raw AS (
    SELECT issue_id, MAX(created_at) AS gate_at
    FROM comment
    WHERE deleted_at IS NULL AND author_type = 'agent'
      AND content ~ '(^|[\n])✅ 验收门[:：]'
    GROUP BY issue_id
), gate AS (
    SELECT g.issue_id, g.gate_at
    FROM gate_raw g
    WHERE NOT EXISTS (SELECT 1 FROM comment h
        WHERE h.issue_id = g.issue_id AND h.deleted_at IS NULL
          AND h.author_type = 'system' AND h.created_at > g.gate_at
          AND (h.type = 'system' OR h.content LIKE '%<!-- handoff:%'))
), lastc AS (
    SELECT DISTINCT ON (issue_id) issue_id, content, created_at
    FROM comment
    WHERE deleted_at IS NULL
    ORDER BY issue_id, created_at DESC
), active AS (
    SELECT DISTINCT issue_id
    FROM agent_task_queue
    WHERE status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred')
), kids AS (
    SELECT c.parent_issue_id,
           COUNT(*) FILTER (WHERE COALESCE(cs.category, CASE
               WHEN c.status = 'done' THEN 'done'
               WHEN c.status = 'cancelled' THEN 'closed'
               ELSE 'started' END) NOT IN ('done', 'closed')) AS unfinished
    FROM issue c
    LEFT JOIN issue_status cs ON cs.workspace_id = c.workspace_id AND cs.key = c.status
    WHERE c.parent_issue_id IS NOT NULL
    GROUP BY c.parent_issue_id
)
SELECT i.id::text, i.status, l.content, g.gate_at,
       COALESCE(la.last_at, g.gate_at) AS last_actor_at
FROM issue i
LEFT JOIN issue_status ist ON ist.workspace_id = i.workspace_id AND ist.key = i.status
JOIN gate g ON g.issue_id = i.id
JOIN lastc l ON l.issue_id = i.id
LEFT JOIN active a ON a.issue_id = i.id
LEFT JOIN kids k ON k.parent_issue_id = i.id
LEFT JOIN LATERAL (
    SELECT MAX(created_at) AS last_at
    FROM comment
    WHERE issue_id = i.id AND deleted_at IS NULL AND author_type IN ('agent', 'member')
) la ON true
WHERE COALESCE(ist.category, CASE
    WHEN i.status IN ('in_progress', 'in_review', 'blocked') THEN 'started'
    ELSE NULL END) = 'started'
  AND i.status <> 'blocked'
  AND a.issue_id IS NULL
  AND ($2::uuid IS NULL OR i.id = $2::uuid)
  AND l.created_at < now() - make_interval(secs => $1)
  AND COALESCE(k.unfinished, 0) = 0
ORDER BY l.created_at
LIMIT 100
`

// gateAutoAdvanceBlocked reports whether the latest comment explicitly says
// work is still waiting or approval is required, regardless of issue status.
func gateAutoAdvanceBlocked(_ string, lastComment string) bool {
	trimmed := strings.TrimSpace(lastComment)
	return strings.HasPrefix(trimmed, "⏳") || strings.HasPrefix(trimmed, "⛔")
}

// gateSupersededByActor reports whether an agent or member spoke after the
// acceptance gate. Human rework instructions must not be auto-accepted.
func gateSupersededByActor(gateAt, lastActorAt time.Time) bool {
	return lastActorAt.After(gateAt)
}

// SweepBarrierLiveness advances every gated, idle issue to `done` and lets the
// normal child-done notification close the stage barrier. It returns the number
// of issues advanced. Called from a server-side ticker; best-effort per row so
// one bad row cannot stall the rest.
func (h *Handler) SweepBarrierLiveness(ctx context.Context) int {
	// A recovered child may have finished while its parent gate was old. Post
	// the result first so candidate selection sees and invalidates that gate.
	h.backfillChildResultHandoffs(ctx)

	rows, err := h.DB.Query(ctx, barrierReadyIssuesSQL, int(barrierSweepIdle.Seconds()), nil)
	if err != nil {
		slog.Warn("barrier sweeper: candidate query failed", "error", err)
		return 0
	}
	var ids []string
	for rows.Next() {
		var id, status, lastComment string
		var gateAt, lastActorAt time.Time
		if err := rows.Scan(&id, &status, &lastComment, &gateAt, &lastActorAt); err != nil {
			slog.Warn("barrier sweeper: scan failed", "error", err)
			continue
		}
		if gateAutoAdvanceBlocked(status, lastComment) {
			continue
		}
		if gateSupersededByActor(gateAt, lastActorAt) {
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
		prev, updated, advanced := h.advanceBarrierCandidate(ctx, id)
		if !advanced {
			continue
		}
		var gate string
		_ = h.DB.QueryRow(ctx,
			`SELECT content FROM comment WHERE issue_id = $1 AND deleted_at IS NULL AND author_type = 'agent' AND content ~ '(^|[\n])✅ 验收门[:：]' ORDER BY created_at DESC LIMIT 1`,
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

// advanceBarrierCandidate locks the issue and rechecks the complete gate
// predicate before changing status. A member reply touches that same issue
// row, so it cannot slip between this check and the status update.
func (h *Handler) advanceBarrierCandidate(ctx context.Context, id pgtype.UUID) (db.Issue, db.Issue, bool) {
	if h.TxStarter == nil {
		return db.Issue{}, db.Issue{}, false
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		slog.Warn("barrier sweeper: begin failed", "issue_id", uuidToString(id), "error", err)
		return db.Issue{}, db.Issue{}, false
	}
	defer tx.Rollback(ctx)
	var lockedID pgtype.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM issue WHERE id = $1 FOR UPDATE`, id).Scan(&lockedID); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("barrier sweeper: lock failed", "issue_id", uuidToString(id), "error", err)
		}
		return db.Issue{}, db.Issue{}, false
	}
	var candidateID, status, lastComment string
	var gateAt, lastActorAt time.Time
	if err := tx.QueryRow(ctx, barrierReadyIssuesSQL, int(barrierSweepIdle.Seconds()), id).
		Scan(&candidateID, &status, &lastComment, &gateAt, &lastActorAt); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("barrier sweeper: recheck failed", "issue_id", uuidToString(id), "error", err)
		}
		return db.Issue{}, db.Issue{}, false
	}
	if gateAutoAdvanceBlocked(status, lastComment) || gateSupersededByActor(gateAt, lastActorAt) {
		return db.Issue{}, db.Issue{}, false
	}
	qtx := h.Queries.WithTx(tx)
	prev, err := qtx.GetIssue(ctx, id)
	if err != nil {
		return db.Issue{}, db.Issue{}, false
	}
	updated, err := qtx.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{
		ID: prev.ID, WorkspaceID: prev.WorkspaceID, Status: "done",
	})
	if err != nil {
		slog.Warn("barrier sweeper: advance failed", "issue_id", uuidToString(id), "error", err)
		return db.Issue{}, db.Issue{}, false
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Warn("barrier sweeper: commit failed", "issue_id", uuidToString(id), "error", err)
		return db.Issue{}, db.Issue{}, false
	}
	return prev, updated, true
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
	category, err := issuestatus.CategoryWithError(ctx, h.issueStatusCatalog(), parent.WorkspaceID, parent.Status)
	if err != nil || category == issuestatus.CategoryDone || category == issuestatus.CategoryClosed || parent.Status == "backlog" {
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
	created, err := h.Queries.CreateComment(ctx, db.CreateCommentParams{
		ID:          dbid.NewV7(),
		IssueID:     parent.ID,
		WorkspaceID: parent.WorkspaceID,
		AuthorType:  "system",
		AuthorID:    pgtype.UUID{Bytes: [16]byte{}, Valid: true},
		Content:     content,
		Type:        "comment",
	})
	if err != nil {
		slog.Warn("barrier sweeper: parent handoff failed", "parent_id", uuidToString(parent.ID), "error", err)
		return
	}
	h.publish(protocol.EventCommentCreated, uuidToString(parent.WorkspaceID), "system", "", map[string]any{
		"comment":             commentToResponse(created.Comment(), nil, nil),
		"issue_title":         parent.Title,
		"issue_assignee_type": textToPtr(parent.AssigneeType),
		"issue_assignee_id":   uuidToPtr(parent.AssigneeID),
		"issue_status":        parent.Status,
		"issue_revision":      created.IssueRevision,
	})
}

// backfillDoneChildrenHandoffsSQL finds done children whose parent is still
// active and that never received this child's handoff comment.
const backfillDoneChildrenHandoffsSQL = `
SELECT c.id::text
FROM issue c
JOIN issue p ON p.id = c.parent_issue_id
LEFT JOIN issue_status cs ON cs.workspace_id = c.workspace_id AND cs.key = c.status
LEFT JOIN issue_status ps ON ps.workspace_id = p.workspace_id AND ps.key = p.status
JOIN LATERAL (
    SELECT 1 FROM comment g
    WHERE g.issue_id = c.id AND g.deleted_at IS NULL AND g.author_type = 'agent'
      AND g.content ~ '(^|[\n])✅ 验收门[:：]'
    LIMIT 1
) gate ON true
WHERE COALESCE(cs.category, CASE WHEN c.status = 'done' THEN 'done' ELSE NULL END) = 'done'
  AND COALESCE(ps.category, CASE
      WHEN p.status IN ('in_progress', 'in_review', 'blocked') THEN 'started'
      ELSE NULL END) = 'started'
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
			`SELECT content FROM comment WHERE issue_id = $1 AND deleted_at IS NULL AND author_type = 'agent' AND content ~ '(^|[\n])✅ 验收门[:：]' ORDER BY created_at DESC LIMIT 1`,
			child.ID).Scan(&gate)
		h.postChildResultToParent(ctx, child, gate)
		posted++
	}
	if posted > 0 {
		slog.Info("barrier sweeper: backfilled child result handoffs", "count", posted)
	}
}
