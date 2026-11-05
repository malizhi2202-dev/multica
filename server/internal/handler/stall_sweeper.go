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

// stallSweepIdle is how long a leaf issue may sit with no run before the
// sweeper re-arms it. It is deliberately shorter than a human round-trip: the
// whole point is that nobody has to notice.
const stallSweepIdle = 15 * time.Minute

// stallSweepDedup is the quiet period after one nudge before the same issue may
// be nudged again. A nudge enqueues a run, so this is also the retry cadence.
const stallSweepDedup = 90 * time.Minute

// stallSweepMaxAttempts bounds mechanical retries before the sweeper hands the
// issue to a human. Three live attempts is enough to show the stall is not
// transient; more would burn tokens on the same wall.
const stallSweepMaxAttempts = 3

// stallChildrenDedup keeps the "never started" reminder from repeating.
const stallChildrenDedup = 6 * time.Hour

// stallSweepBatch bounds how many issues one tick may wake. The daemon enforces
// its own parallelism caps; this only keeps a backlog from all waking at once.
const stallSweepBatch = 3

// legalParkPrefixes are the ONLY comment openings that legitimately leave an
// issue waiting on a human. Anything else that merely starts with ⏳/⛔ is
// agent shorthand for "this round failed" or "the gate rejected me", and must
// NOT excuse the issue from liveness sweeps — that is how a fleet of issues
// froze silently while every inspector round reported "no anomalies".
var legalParkPrefixes = []string{
	"⏳ 等待",
	"⛔ 需要人工审批",
}

// isLegalParkComment reports whether content is a sanctioned wait marker.
func isLegalParkComment(content string) bool {
	trimmed := strings.TrimSpace(content)
	for _, prefix := range legalParkPrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

// stalledLeafIssuesSQL finds active leaf issues that stopped moving: no queued
// or running task, no unfinished child to wait on, and the newest comment is an
// agent's own text from longer ago than the idle window.
//
// Excluding issues with unfinished children is what separates a stalled issue
// from a parent that is correctly parked on its stage barrier.
const stalledLeafIssuesSQL = `
WITH lastc AS (
    SELECT DISTINCT ON (issue_id) issue_id, content, created_at, author_type
    FROM comment
    WHERE deleted_at IS NULL
    ORDER BY issue_id, created_at DESC
), active AS (
    SELECT DISTINCT issue_id
    FROM agent_task_queue
    WHERE status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
), nudged AS (
    SELECT issue_id, COUNT(*) AS n
    FROM comment
    WHERE deleted_at IS NULL AND content LIKE '%<!-- stall-sweep:%'
      AND created_at > now() - make_interval(secs => $2)
    GROUP BY issue_id
), attempts AS (
    SELECT issue_id, COUNT(*) AS n
    FROM comment
    WHERE deleted_at IS NULL AND content LIKE '%<!-- stall-sweep:%'
    GROUP BY issue_id
)
SELECT i.id::text, i.status, l.content, l.created_at,
       COALESCE(att.n, 0) AS attempts
FROM issue i
JOIN lastc l ON l.issue_id = i.id
LEFT JOIN active a ON a.issue_id = i.id
LEFT JOIN nudged n ON n.issue_id = i.id
LEFT JOIN attempts att ON att.issue_id = i.id
WHERE a.issue_id IS NULL
  AND n.issue_id IS NULL
  AND l.author_type = 'agent'
  AND l.created_at < now() - make_interval(secs => $1)
  AND i.status IN ('in_progress', 'blocked')
  AND i.assignee_type = 'agent' AND i.assignee_id IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM issue c
      WHERE c.parent_issue_id = i.id AND c.status NOT IN ('done', 'cancelled')
  )
  AND NOT EXISTS (
      SELECT 1 FROM comment e
      WHERE e.issue_id = i.id AND e.deleted_at IS NULL
        AND e.content LIKE '%<!-- stall-escalated:%'
  )
ORDER BY l.created_at
LIMIT $3
`

// stalledCandidate is one issue the sweep may re-arm.
type stalledCandidate struct {
	ID            string
	Status        string
	LastComment   string
	LastCommentAt time.Time
	Attempts      int
}

// parentStuckOnUnstartedChildSQL finds the deadlock shape the leaf sweep cannot
// see: a parent waiting on exactly one unfinished child that never ran.
//
// A parent is normally excluded from the leaf sweep because waiting on children
// is its job. But a child parked in backlog/todo has no run, no comment, and no
// owner watching it — and only done/cancelled close a stage barrier — so the
// parent can wait forever for something nobody will start. Waking the parent is
// the right move: it can promote the child or cancel it with a reason.
const parentStuckOnUnstartedChildSQL = `
WITH lastc AS (
    SELECT DISTINCT ON (issue_id) issue_id, content, created_at, author_type
    FROM comment
    WHERE deleted_at IS NULL
    ORDER BY issue_id, created_at DESC
), active AS (
    SELECT DISTINCT issue_id
    FROM agent_task_queue
    WHERE status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
), nudged AS (
    SELECT DISTINCT issue_id
    FROM comment
    WHERE deleted_at IS NULL AND content LIKE '%<!-- stall-parent:%'
      AND created_at > now() - make_interval(secs => $2)
)
SELECT p.id::text, p.status, l.content, l.created_at
FROM issue p
JOIN lastc l ON l.issue_id = p.id
LEFT JOIN active a ON a.issue_id = p.id
LEFT JOIN nudged n ON n.issue_id = p.id
WHERE a.issue_id IS NULL AND n.issue_id IS NULL
  AND p.status IN ('in_progress', 'in_review')
  AND p.assignee_type IN ('agent', 'squad') AND p.assignee_id IS NOT NULL
  AND l.author_type = 'agent'
  AND l.created_at < now() - make_interval(secs => $1)
  AND NOT EXISTS (
      SELECT 1 FROM comment e
      WHERE e.issue_id = p.id AND e.deleted_at IS NULL
        AND e.content LIKE '%<!-- stall-escalated:%'
  )
  AND (SELECT COUNT(*) FROM issue c
       WHERE c.parent_issue_id = p.id AND c.status NOT IN ('done', 'cancelled')) = 1
  AND EXISTS (
      SELECT 1 FROM issue c
      WHERE c.parent_issue_id = p.id AND c.status IN ('backlog', 'todo')
        AND NOT EXISTS (SELECT 1 FROM agent_task_queue t WHERE t.issue_id = c.id)
  )
ORDER BY l.created_at
LIMIT $3
`

// SweepStalledIssues is the mechanical half of liveness: it does not judge
// output, it just refuses to let an active issue sit with nobody working on it.
//
// Three shapes are handled:
//
//   - `blocked` with a run in flight. The status contradicts the run, and a
//     blocked issue is invisible to every retry path, so it is corrected to
//     `in_progress` rather than left to a human to notice.
//   - `in_progress`/`blocked` with no run, a leaf, and an agent-authored last
//     comment that is not a sanctioned wait marker. The assignee is re-armed
//     with a comment naming the concrete unblock options, and a run is enqueued
//     so the wake-up does not depend on a mention being read.
//   - a parent whose last unfinished child was never started. Waiting on a
//     never-started child is not progress, so the parent is woken to promote or
//     cancel it.
//
// The decision is made from a comment prefix and the task table only, so it does
// not depend on an inspector model noticing the stall — that model reported "no
// anomalies" while an issue sat frozen for over an hour.
//
// After stallSweepMaxAttempts mechanical retries the issue is handed to a
// human instead of being retried again.
func (h *Handler) SweepStalledIssues(ctx context.Context) int {
	h.reconcileBlockedIssues(ctx)

	rows, err := h.DB.Query(ctx, stalledLeafIssuesSQL,
		int(stallSweepIdle.Seconds()), int(stallSweepDedup.Seconds()), stallSweepBatch)
	if err != nil {
		slog.Warn("stall sweeper: candidate query failed", "error", err)
		return 0
	}
	var candidates []stalledCandidate
	for rows.Next() {
		var c stalledCandidate
		if err := rows.Scan(&c.ID, &c.Status, &c.LastComment, &c.LastCommentAt, &c.Attempts); err != nil {
			slog.Warn("stall sweeper: scan failed", "error", err)
			continue
		}
		candidates = append(candidates, c)
	}
	rows.Close()

	rearmed := 0
	for _, c := range candidates {
		id, err := util.ParseUUID(c.ID)
		if err != nil {
			continue
		}
		issue, err := h.Queries.GetIssue(ctx, id)
		if err != nil {
			continue
		}
		switch issue.Status {
		case "in_progress", "blocked":
		default:
			continue
		}
		// The sanctioned wait markers are the only comments that should sit
		// unresolved; they are escalated by the timeout path, not re-armed here.
		if isLegalParkComment(c.LastComment) {
			continue
		}
		mention := h.issueAssigneeMention(ctx, issue)
		minutes := int(time.Since(c.LastCommentAt).Minutes())
		if minutes < 1 {
			minutes = 1
		}

		if c.Attempts >= stallSweepMaxAttempts {
			h.postSweepComment(ctx, issue, fmt.Sprintf(
				"%s停滞移交：本 issue 已自动续跑 %d 次仍在 `%s` 且无运行任务（最后一次进展在 %d 分钟前），机械重试到此为止，交给人工判断。\n"+
					"请决定：继续推进（可让我按需重跑）、改 `cancelled` 记录原因、或改写验收口径。\n<!-- stall-escalated:%s -->",
				mention, c.Attempts, issue.Status, minutes, c.ID))
			slog.Info("stall sweeper: handed to human", "issue_id", c.ID, "attempts", c.Attempts)
			continue
		}

		var body string
		if issue.Status == "blocked" {
			body = fmt.Sprintf(
				"%s阻塞复核：本 issue 标为 `blocked`，已 %d 分钟没有运行任务（服务端机械巡检，不判断对错）。\n"+
					"`blocked` 只用于「等外部依赖且当前无法自决」；环境缺失、缺上游产物、缺一票都不算。请本轮二选一：\n"+
					"- 能自决 → 置回 `in_progress` 继续（缺环境自建本地等价替代并标注「本地替代环境」；缺基线用 `multica repo checkout <repo> --ref <上游交付分支>` 重建，分支名见 `multica issue runs <父 issue>`）。\n"+
					"- 真需要人 → 改成 `⏳ 等待确认 <具体问题>` 或 `⛔ 需要人工审批 <红线> + 影响 + 默认建议`，并写明谁/什么条件能解锁。\n"+
					"<!-- stall-sweep:%s -->", mention, minutes, c.ID)
		} else {
			body = fmt.Sprintf(
				"%s停滞自检：本 issue 已 %d 分钟没有运行任务，仍为 `in_progress`（服务端机械巡检，不判断对错）。\n"+
					"本轮直接推进，不要只写「下一步」：\n"+
					"- 缺上游产物（读不到 `.specs` 的 TASK/DESIGN、上游包目录）→ 用 `multica repo checkout <repo> --ref <上游交付分支>` 重建基线（分支名见 `multica issue runs <父 issue>`）；仍缺就按最小完整改动降级实现并标注。\n"+
					"- 缺环境（DB/认证/日志连不上）→ 自建本地等价替代（本地 DB、合成身份与密钥、mock）走通同一验收路径，证据标注「本地替代环境」；只有不可逆/越权/生产动作才停。\n"+
					"- 收口用 `✅ 验收门` 后转 `in_review`；确实要人等写 `⏳ 等待确认 <问题>`，需审批写 `⛔ 需要人工审批 <红线>`。除此之外不要以 `⏳`/`⛔` 开头收尾。\n"+
					"<!-- stall-sweep:%s -->", mention, minutes, c.ID)
		}
		commentID := h.postSweepComment(ctx, issue, body)
		if commentID == nil {
			continue
		}
		// The comment carries the instruction; the run is what makes it land.
		// A system comment alone does not enqueue anything, so a nudge without
		// this call would look like progress and change nothing.
		if _, err := h.TaskService.RerunIssue(ctx, issue.ID, pgtype.UUID{}, *commentID, pgtype.UUID{},
			func(db.Agent) bool { return true }); err != nil {
			slog.Warn("stall sweeper: rerun failed", "issue_id", c.ID, "error", err)
			continue
		}
		rearmed++
	}

	rearmed += h.sweepParentsOnUnstartedChild(ctx)
	if rearmed > 0 {
		slog.Info("stall sweeper: re-armed idle issues", "count", rearmed)
	}
	return rearmed
}

// reconcileBlockedIssues fixes statuses that contradict an in-flight run.
//
// A `blocked` issue with a run in flight is stale by definition: the agent is
// working on it. Leaving it blocked hides the issue from retry paths (they look
// at in_progress) and from every sweep, so the contradiction is corrected
// mechanically instead of waiting for a human to type "where is it blocked?".
func (h *Handler) reconcileBlockedIssues(ctx context.Context) {
	const sqlText = `
WITH active AS (
    SELECT DISTINCT issue_id
    FROM agent_task_queue
    WHERE status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
)
SELECT i.id::text
FROM issue i
JOIN active a ON a.issue_id = i.id
WHERE i.status = 'blocked'
LIMIT 50
`
	rows, err := h.DB.Query(ctx, sqlText)
	if err != nil {
		slog.Warn("stall sweeper: blocked reconcile query failed", "error", err)
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

	for _, raw := range ids {
		id, err := util.ParseUUID(raw)
		if err != nil {
			continue
		}
		issue, err := h.Queries.GetIssue(ctx, id)
		if err != nil || issue.Status != "blocked" {
			continue
		}
		if _, err := h.Queries.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{
			ID:          issue.ID,
			WorkspaceID: issue.WorkspaceID,
			Status:      "in_progress",
		}); err != nil {
			slog.Warn("stall sweeper: unblock failed", "issue_id", raw, "error", err)
			continue
		}
		h.postSweepComment(ctx, issue, fmt.Sprintf(
			"状态纠正：本 issue 标为 `blocked`，但已有运行任务在执行，已自动置回 `in_progress`（服务端 stall sweeper）。\n"+
				"`blocked` 只用于「等外部依赖且当前无法自决」；继续工作时请不要保留该状态。\n<!-- stall-status:%s -->",
			raw))
		slog.Info("stall sweeper: reconciled blocked issue with active run", "issue_id", raw)
	}
}

// sweepParentsOnUnstartedChild wakes a parent whose only remaining child was
// never started, naming the child so the decision is concrete.
func (h *Handler) sweepParentsOnUnstartedChild(ctx context.Context) int {
	rows, err := h.DB.Query(ctx, parentStuckOnUnstartedChildSQL,
		int(stallSweepIdle.Seconds()), int(stallSweepDedup.Seconds()), stallSweepBatch)
	if err != nil {
		slog.Warn("stall sweeper: parent query failed", "error", err)
		return 0
	}
	type parentRow struct {
		id            string
		status        string
		lastComment   string
		lastCommentAt time.Time
	}
	var parents []parentRow
	for rows.Next() {
		var p parentRow
		if err := rows.Scan(&p.id, &p.status, &p.lastComment, &p.lastCommentAt); err != nil {
			continue
		}
		parents = append(parents, p)
	}
	rows.Close()

	woken := 0
	for _, p := range parents {
		id, err := util.ParseUUID(p.id)
		if err != nil {
			continue
		}
		parent, err := h.Queries.GetIssue(ctx, id)
		if err != nil {
			continue
		}
		if isLegalParkComment(p.lastComment) {
			continue
		}
		childLabel := h.onlyUnstartedChildLabel(ctx, parent)
		if childLabel == "" {
			continue
		}
		mention := h.issueAssigneeMention(ctx, parent)
		minutes := int(time.Since(p.lastCommentAt).Minutes())
		if minutes < 1 {
			minutes = 1
		}
		commentID := h.postSweepComment(ctx, parent, fmt.Sprintf(
			"%s子任务未启动：本 issue 只剩 %s 未终结，而它处于 `backlog`/`todo` 且从未运行过，已 %d 分钟没有运行任务（服务端机械巡检）。\n"+
				"只有 `done`/`cancelled` 算终结 → 现在的屏障等的是一个没人启动的子任务。请本轮决定：置 `in_progress` 开工；或置 `cancelled` 并写明原因（可重建）；确实要人等就写 `⏳ 等待确认 <问题>`。\n"+
				"<!-- stall-parent:%s -->", mention, childLabel, minutes, p.id))
		if commentID == nil {
			continue
		}
		if _, err := h.TaskService.RerunIssue(ctx, parent.ID, pgtype.UUID{}, *commentID, pgtype.UUID{},
			func(db.Agent) bool { return true }); err != nil {
			slog.Warn("stall sweeper: parent rerun failed", "issue_id", p.id, "error", err)
			continue
		}
		woken++
	}
	return woken
}

// onlyUnstartedChildLabel renders the single unfinished never-ran child, or ""
// when the shape changed since the query ran.
func (h *Handler) onlyUnstartedChildLabel(ctx context.Context, parent db.Issue) string {
	rows, err := h.DB.Query(ctx,
		`SELECT id::text, number, title FROM issue
		 WHERE parent_issue_id = $1 AND status NOT IN ('done', 'cancelled')
		 ORDER BY number`, parent.ID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var (
		ids     []string
		numbers []int32
		titles  []string
	)
	for rows.Next() {
		var id, title string
		var number int32
		if err := rows.Scan(&id, &number, &title); err != nil {
			return ""
		}
		ids = append(ids, id)
		numbers = append(numbers, number)
		titles = append(titles, title)
	}
	if err := rows.Err(); err != nil || len(ids) != 1 {
		return ""
	}
	var started bool
	if err := h.DB.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM agent_task_queue WHERE issue_id = $1)`, ids[0]).Scan(&started); err != nil {
		return ""
	}
	if started {
		return ""
	}
	return fmt.Sprintf("[#%d %s](mention://issue/%s)", numbers[0], titles[0], ids[0])
}

// issueAssigneeMention renders the assignee mention that carries the wake-up.
func (h *Handler) issueAssigneeMention(ctx context.Context, issue db.Issue) string {
	if !issue.AssigneeType.Valid || !issue.AssigneeID.Valid {
		return ""
	}
	switch issue.AssigneeType.String {
	case "agent":
		if ag, err := h.Queries.GetAgent(ctx, issue.AssigneeID); err == nil {
			return fmt.Sprintf("[@%s](mention://agent/%s) ", ag.Name, uuidToString(issue.AssigneeID))
		}
	case "squad":
		if sq, err := h.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{
			ID:          issue.AssigneeID,
			WorkspaceID: issue.WorkspaceID,
		}); err == nil {
			return fmt.Sprintf("[@%s](mention://squad/%s) ", sq.Name, uuidToString(issue.AssigneeID))
		}
	}
	return ""
}

// postSweepComment writes one system comment and returns its ID so a caller can
// bind a queued run to it.
func (h *Handler) postSweepComment(ctx context.Context, issue db.Issue, content string) *pgtype.UUID {
	id := dbid.NewV7()
	if _, err := h.Queries.CreateComment(ctx, db.CreateCommentParams{
		ID:          id,
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
		AuthorType:  "system",
		AuthorID:    pgtype.UUID{Bytes: [16]byte{}, Valid: true},
		Content:     content,
		Type:        "comment",
	}); err != nil {
		slog.Warn("stall sweeper: comment failed", "issue_id", uuidToString(issue.ID), "error", err)
		return nil
	}
	return &id
}
