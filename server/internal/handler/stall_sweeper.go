package handler

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// stallSweepIdle is how long a leaf issue may sit with no run before the
// sweeper re-arms it. It is deliberately shorter than a human round-trip: the
// whole point is that nobody has to notice.
const stallSweepIdle = 15 * time.Minute

// stallSweepRetryIgnored is how long a nudge the assignee never answered is
// left alone before it is sent again. A nudge enqueues a run, so this is the
// retry cadence for a run that produced no reply at all.
const stallSweepRetryIgnored = 2 * time.Hour

// stallSweepMaxAttempts bounds mechanical retries before the sweeper hands the
// issue to a human. Three live attempts is enough to show the stall is not
// transient; more would burn tokens on the same wall.
const stallSweepMaxAttempts = 3

// stallSweepEscalate is how long a child may stay parked on its own executor's
// word before the decision is handed to the parent's leader. A parked member is
// not a decision: it is a question the wrong level was asked.
const stallSweepEscalate = 5 * time.Minute

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
), last_nudge AS (
    SELECT issue_id, MAX(created_at) AS at
    FROM comment
    WHERE deleted_at IS NULL AND content LIKE '%<!-- stall-sweep:%'
    GROUP BY issue_id
), attempts AS (
    SELECT issue_id, COUNT(*) AS n
    FROM comment
    WHERE deleted_at IS NULL AND content LIKE '%<!-- stall-sweep:%'
    GROUP BY issue_id
)
SELECT i.id::text, i.status, l.content, l.created_at,
       COALESCE(att.n, 0) AS attempts,
       (l.content LIKE '%mention://%') AS has_mention
FROM issue i
JOIN lastc l ON l.issue_id = i.id
LEFT JOIN active a ON a.issue_id = i.id
LEFT JOIN last_nudge n ON n.issue_id = i.id
LEFT JOIN attempts att ON att.issue_id = i.id
WHERE a.issue_id IS NULL
  -- One nudge per silent stretch, not one per clock window: a round that ended
  -- without dispatching anything and then went quiet must be re-armed again,
  -- while a nudge the assignee ignored is retried only after a long backoff.
  AND (n.at IS NULL OR n.at < l.created_at OR n.at < now() - make_interval(secs => $3))
  AND l.author_type = 'agent'
  -- Two idles: a turn that handed off to nobody is a dead end within minutes,
  -- while a turn that did mention someone gets the longer window in case the
  -- dispatch it asked for is still being claimed.
  AND l.created_at < now() - make_interval(secs => LEAST($1::int, $4::int))
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
LIMIT $2
`

// stalledCandidate is one issue the sweep may re-arm.
type stalledCandidate struct {
	ID            string
	Status        string
	LastComment   string
	LastCommentAt time.Time
	Attempts      int
	// HasMention reports whether the last word mentioned anyone. A turn that
	// ended without a mention handed the issue to nobody.
	HasMention bool
}

// stallSweepDeadEnd is how long a turn that dispatched nobody is given before
// it is re-armed. It is deliberately short: such a turn did not ask anyone for
// anything, so there is no work in flight to wait for.
const stallSweepDeadEnd = 3 * time.Minute

// deadEndIdle is the quiet time this candidate must have before re-arming.
func deadEndIdle(hasMention bool) time.Duration {
	if hasMention {
		return stallSweepIdle
	}
	return stallSweepDeadEnd
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
), last_nudge AS (
    SELECT issue_id, MAX(created_at) AS at
    FROM comment
    WHERE deleted_at IS NULL AND content LIKE '%<!-- stall-parent:%'
    GROUP BY issue_id
)
SELECT p.id::text, p.status, l.content, l.created_at
FROM issue p
JOIN lastc l ON l.issue_id = p.id
LEFT JOIN active a ON a.issue_id = p.id
LEFT JOIN last_nudge n ON n.issue_id = p.id
WHERE a.issue_id IS NULL
  AND (n.at IS NULL OR n.at < l.created_at OR n.at < now() - make_interval(secs => $3))
  AND p.status IN ('in_progress', 'in_review')
  AND p.assignee_type IN ('agent', 'squad') AND p.assignee_id IS NOT NULL
  AND l.author_type = 'agent'
  -- Two idles: a turn that handed off to nobody is a dead end within minutes,
  -- while a turn that did mention someone gets the longer window in case the
  -- dispatch it asked for is still being claimed.
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
LIMIT $2
`

// stalledReviewIssuesSQL finds `in_review` leaves whose review state cannot
// resolve itself: either the acceptance gate was superseded by a later agent
// comment (so the auto-close correctly refuses it), or the issue was moved to
// review with no gate at all.
//
// Both shapes need the assignee, not a gate sweep: the automated close must not
// guess, and nobody else is going to speak for the issue. A review that had its
// gate superseded could otherwise sit open forever, holding its parent's stage.
const stalledReviewIssuesSQL = `
WITH lastc AS (
    SELECT DISTINCT ON (issue_id) issue_id, content, created_at, author_type
    FROM comment
    WHERE deleted_at IS NULL
    ORDER BY issue_id, created_at DESC
), active AS (
    SELECT DISTINCT issue_id
    FROM agent_task_queue
    WHERE status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
), last_nudge AS (
    SELECT issue_id, MAX(created_at) AS at
    FROM comment
    WHERE deleted_at IS NULL AND content LIKE '%<!-- stall-review:%'
    GROUP BY issue_id
), gates AS (
    SELECT issue_id, MAX(created_at) AS gate_at, COUNT(*) AS n
    FROM comment
    WHERE deleted_at IS NULL AND content LIKE '✅ 验收门%'
    GROUP BY issue_id
)
SELECT i.id::text, l.content, l.created_at,
       COALESCE(g.n, 0) AS gates,
       (SELECT COUNT(*) FROM comment a
        WHERE a.issue_id = i.id AND a.deleted_at IS NULL AND a.author_type = 'agent'
          AND a.created_at > COALESCE(g.gate_at, 'epoch'::timestamptz)) AS agent_after_gate
FROM issue i
JOIN lastc l ON l.issue_id = i.id
LEFT JOIN active a ON a.issue_id = i.id
LEFT JOIN last_nudge nd ON nd.issue_id = i.id
LEFT JOIN gates g ON g.issue_id = i.id
WHERE a.issue_id IS NULL
  AND (nd.at IS NULL OR nd.at < l.created_at OR nd.at < now() - make_interval(secs => $3))
  AND i.status = 'in_review'
  AND i.assignee_type = 'agent' AND i.assignee_id IS NOT NULL
  AND l.author_type = 'agent'
  -- Two idles: a turn that handed off to nobody is a dead end within minutes,
  -- while a turn that did mention someone gets the longer window in case the
  -- dispatch it asked for is still being claimed.
  AND l.created_at < now() - make_interval(secs => $1)
  AND NOT EXISTS (
      SELECT 1 FROM issue c
      WHERE c.parent_issue_id = i.id AND c.status NOT IN ('done', 'cancelled')
  )
ORDER BY l.created_at
LIMIT $2
`

// reviewNudgeKind names the mechanical reason a review cannot close itself.
// "gate" means an agent spoke after the newest gate, so that gate is history;
// "missing_gate" means the issue was moved to review without one.
func reviewNudgeKind(gates, agentAfterGate int) string {
	if gates == 0 {
		return "missing_gate"
	}
	if agentAfterGate > 0 {
		return "gate"
	}
	return ""
}

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
		int(stallSweepIdle.Seconds()), stallSweepBatch, int(stallSweepRetryIgnored.Seconds()),
		int(stallSweepDeadEnd.Seconds()))
	if err != nil {
		slog.Warn("stall sweeper: candidate query failed", "error", err)
		return 0
	}
	var candidates []stalledCandidate
	for rows.Next() {
		var c stalledCandidate
		if err := rows.Scan(&c.ID, &c.Status, &c.LastComment, &c.LastCommentAt, &c.Attempts, &c.HasMention); err != nil {
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
		if time.Since(c.LastCommentAt) < deadEndIdle(c.HasMention) {
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
		if !c.HasMention {
			// The specific anti-pattern: a polite, complete-looking report that
			// hands the issue to nobody. "请负责人/维护者…" names a role, not an
			// agent, so no run is queued by it.
			body = fmt.Sprintf(
				"%s收尾没有接棒人：上一轮结束时既没有 mention 任何 agent，也不是 `⏳ 等待…`/`⛔ 需要人工审批…`，所以没有人会被唤醒（服务端机械巡检，%d 分钟前）。\n"+
					"本轮必须补上接棒动作，三选一：\n"+
					"- **能自决** → 直接做：缺环境自建本地替代并标注「本地替代环境」；缺上游产物先 `git merge <上游交付分支>`；写集不够在工作区内申报扩张继续做；缺票就 **mention 每一位投票人**真的发起。\n"+
					"- **要 agent 接** → `[@名字](mention://agent/<id>)` 真的派出去（写「下一步派 X」不算）。\n"+
					"- **要人等** → `⏳ 等待确认 <具体问题 + 默认建议 + 影响>`；要审批 → `⛔ 需要人工审批 <红线 + 影响 + 默认建议>`。\n"+
					"只写结论/请「负责人」「维护者」「你们」处理 = 本轮没人接手。\n"+
					"<!-- stall-sweep:%s -->", mention, minutes, c.ID)
		} else if issue.Status == "blocked" {
			body = fmt.Sprintf(
				"%s阻塞复核：本 issue 标为 `blocked`，已 %d 分钟没有运行任务（服务端机械巡检，不判断对错）。\n"+
					"`blocked` 只用于「等外部依赖且当前无法自决」；环境缺失、缺上游产物、缺一票都不算。请本轮二选一：\n"+
					"- 能自决 → 置回 `in_progress` 继续（缺环境自建本地等价替代并标注「本地替代环境」；缺上游产物先 `git merge <上游交付分支>` 把父级产物并入本任务分支，分支名见 `multica issue runs <父 issue>`）。\n"+
					"- 真需要人 → 改成 `⏳ 等待确认 <具体问题>` 或 `⛔ 需要人工审批 <红线> + 影响 + 默认建议`，并写明谁/什么条件能解锁。\n"+
					"<!-- stall-sweep:%s -->", mention, minutes, c.ID)
		} else {
			body = fmt.Sprintf(
				"%s停滞自检：本 issue 已 %d 分钟没有运行任务，仍为 `in_progress`（服务端机械巡检，不判断对错）。\n"+
					"本轮直接推进，不要只写「下一步」：\n"+
					"- 缺上游产物（读不到 `.specs` 的 TASK/DESIGN、上游包目录、上游改动）→ 子任务与父级不共享工作树，先把父级已交付分支 `git merge` 进本任务分支再动手（分支名见 `multica issue runs <父 issue>`）；还没有本任务分支时用 `multica repo checkout <repo> --ref <上游交付分支>` 建基线。仍缺才降级实现并标注。\n"+
					"- 缺环境（DB/认证/日志连不上）→ 自建本地等价替代（本地 DB、合成身份与密钥、mock）走通同一验收路径，证据标注「本地替代环境」；只有不可逆/越权/生产动作才停。\n"+
					"- 收口用 `✅ 验收门` 后转 `in_review`；确实要人等写 `⏳ 等待确认 <问题>`，需审批写 `⛔ 需要人工审批 <红线>`。除此之外不要以 `⏳`/`⛔` 开头收尾。\n"+
					"- **收尾必须真的有人被唤醒**：只写结论/请某人确认而不 `mention` 该 agent、也不写等待标记 = 本轮没人接手，issue 会停在这里。要投票就 mention 每位投票人，要授权就 mention 负责人或写 `⛔ 需要人工审批`。\n"+
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

	rearmed += h.sweepStalledReviews(ctx)
	rearmed += h.sweepEscalations(ctx)
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
		int(stallSweepIdle.Seconds()), stallSweepBatch, int(stallSweepRetryIgnored.Seconds()))
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

// sweepStalledReviews re-arms reviews that no longer have a closable gate.
func (h *Handler) sweepStalledReviews(ctx context.Context) int {
	rows, err := h.DB.Query(ctx, stalledReviewIssuesSQL,
		int(stallSweepIdle.Seconds()), stallSweepBatch, int(stallSweepRetryIgnored.Seconds()))
	if err != nil {
		slog.Warn("stall sweeper: review query failed", "error", err)
		return 0
	}
	type reviewRow struct {
		id             string
		lastComment    string
		lastCommentAt  time.Time
		gates          int
		agentAfterGate int
	}
	var reviews []reviewRow
	for rows.Next() {
		var r reviewRow
		if err := rows.Scan(&r.id, &r.lastComment, &r.lastCommentAt, &r.gates, &r.agentAfterGate); err != nil {
			continue
		}
		reviews = append(reviews, r)
	}
	rows.Close()

	woken := 0
	for _, r := range reviews {
		kind := reviewNudgeKind(r.gates, r.agentAfterGate)
		if kind == "" {
			continue
		}
		id, err := util.ParseUUID(r.id)
		if err != nil {
			continue
		}
		issue, err := h.Queries.GetIssue(ctx, id)
		if err != nil || issue.Status != "in_review" {
			continue
		}
		mention := h.issueAssigneeMention(ctx, issue)
		minutes := int(time.Since(r.lastCommentAt).Minutes())
		if minutes < 1 {
			minutes = 1
		}
		var reason string
		if kind == "gate" {
			reason = "`✅ 验收门` 之后又出现了智能体评论，旧门禁因此变成历史记录，自动收口不会再依据它"
		} else {
			reason = "本 issue 处于 `in_review` 但整个线程没有 `✅ 验收门` 记录"
		}
		commentID := h.postSweepComment(ctx, issue, fmt.Sprintf(
			"%s验收门待补：%s（已 %d 分钟无运行任务，服务端机械巡检）。\n"+
				"请本轮二选一：\n"+
				"- 已满足验收 → 重新发一条 `✅ 验收门：AC n/n；TEST/UAT 已附；diff 未越界` 作为本 issue 的**最后一条**评论，服务端随后会自动置 `done` 并回传父级。\n"+
				"- 未满足 → 置回 `in_progress` 继续，或写 `⏳ 等待确认 <问题>` / `⛔ 需要人工审批 <红线>`。\n"+
				"<!-- stall-review:%s -->", mention, reason, minutes, r.id))
		if commentID == nil {
			continue
		}
		if _, err := h.TaskService.RerunIssue(ctx, issue.ID, pgtype.UUID{}, *commentID, pgtype.UUID{},
			func(db.Agent) bool { return true }); err != nil {
			slog.Warn("stall sweeper: review rerun failed", "issue_id", r.id, "error", err)
			continue
		}
		woken++
	}
	return woken
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

// issueBaseRef returns the branch a child issue's worktree should start from:
// the most recently delivered branch of its parent issue.
//
// The structural reason is generic, not tied to one workflow. Every task gets
// its own checkout, branched from the repository default, so a stage child
// never sees what its parent delivered afterwards — the specs, packages and
// migrations the parent landed are simply absent, and the child reports
// (correctly) that it cannot implement against them. Naming the parent's
// delivered branch as the starting point is what makes a task sequence
// cumulative instead of a set of disconnected forks.
//
// Only the parent's OWN branch is used, never a sibling's: a sibling's branch
// carries work that was never integrated under the parent, so basing a child on
// it would present unreviewed work as the baseline. An empty result means "start
// from the default branch", which is exactly the previous behaviour.
func (h *Handler) issueBaseRef(ctx context.Context, issue db.Issue) string {
	if !issue.ParentIssueID.Valid {
		return ""
	}
	const q = `
SELECT COALESCE(NULLIF(t.branch_name, ''), '')
FROM agent_task_queue t
WHERE t.issue_id = $1
  AND t.status = 'completed'
  AND t.completed_at IS NOT NULL
  AND t.completed_at > now() - interval '14 days'
  AND NULLIF(t.branch_name, '') IS NOT NULL
ORDER BY t.completed_at DESC
LIMIT 1`
	var branch string
	if err := h.DB.QueryRow(ctx, q, issue.ParentIssueID).Scan(&branch); err != nil {
		return ""
	}
	return strings.TrimSpace(branch)
}

// escalatedChildIssuesSQL finds children that stopped on their own assignee's
// word while the decision belongs one level up.
//
// A stage child that cannot decide has one useful move: hand the decision to
// the parent's leader, on its own issue, so the leader can widen the write set,
// reorder or close. Parking in `⏳ 等待确认` asks a human directly, which is the
// wrong door for a member and leaves the issue waiting while the parent keeps
// discussing at a level the child never sees.
const escalatedChildIssuesSQL = `
WITH lastc AS (
    SELECT DISTINCT ON (issue_id) issue_id, content, created_at, author_type, author_id
    FROM comment
    WHERE deleted_at IS NULL
    ORDER BY issue_id, created_at DESC
), active AS (
    SELECT DISTINCT issue_id
    FROM agent_task_queue
    WHERE status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
), last_escalation AS (
    SELECT issue_id, MAX(created_at) AS at
    FROM comment
    WHERE deleted_at IS NULL AND content LIKE '%<!-- escalate-up:%'
    GROUP BY issue_id
), human_reply_after AS (
    SELECT DISTINCT c.issue_id
    FROM comment c
    JOIN comment l ON l.issue_id = c.issue_id AND c.created_at > l.created_at
    WHERE c.deleted_at IS NULL AND c.author_type = 'member'
)
SELECT i.id::text, i.parent_issue_id::text, l.content, l.created_at,
       EXTRACT(EPOCH FROM (now() - l.created_at))::int AS age_seconds
FROM issue i
JOIN lastc l ON l.issue_id = i.id
LEFT JOIN active a ON a.issue_id = i.id
LEFT JOIN last_escalation e ON e.issue_id = i.id
WHERE a.issue_id IS NULL
  AND i.status IN ('in_progress', 'blocked')
  AND i.parent_issue_id IS NOT NULL
  AND i.assignee_type = 'agent' AND i.assignee_id IS NOT NULL
  AND l.author_type = 'agent' AND l.author_id = i.assignee_id
  AND (l.content LIKE '⏳%' OR l.content LIKE '⛔%')
  -- A park that names someone is a decision someone was actually given: its own
  -- task agent, a squad mate, or the workspace owner. Those are legitimate for
  -- in-scope questions, so only a park that mentions NOBODY is escalated a level
  -- up — that is the shape with no one left to answer it.
  AND l.content NOT LIKE '%mention://%'
  AND l.created_at < now() - make_interval(secs => $1)
  AND (e.at IS NULL OR e.at < l.created_at OR e.at < now() - make_interval(secs => $3))
ORDER BY l.created_at
LIMIT $2
`

// sweepEscalations hands a parked child's decision to the parent's leader.
// One level up, on the child, is the only place the decision can be made
// without the child waiting for a conversation it cannot see.
func (h *Handler) sweepEscalations(ctx context.Context) int {
	rows, err := h.DB.Query(ctx, escalatedChildIssuesSQL,
		int(stallSweepEscalate.Seconds()), stallSweepBatch, int(stallSweepRetryIgnored.Seconds()))
	if err != nil {
		slog.Warn("stall sweeper: escalation query failed", "error", err)
		return 0
	}
	type escalation struct {
		childID   string
		parentID  string
		lastBody  string
		lastAt    time.Time
		ageSecond int
	}
	var list []escalation
	for rows.Next() {
		var e escalation
		if err := rows.Scan(&e.childID, &e.parentID, &e.lastBody, &e.lastAt, &e.ageSecond); err != nil {
			slog.Warn("stall sweeper: escalation scan failed", "error", err)
			continue
		}
		list = append(list, e)
	}
	rows.Close()

	raised := 0
	for _, e := range list {
		childID, err := util.ParseUUID(e.childID)
		if err != nil {
			continue
		}
		parentID, err := util.ParseUUID(e.parentID)
		if err != nil {
			continue
		}
		child, err := h.Queries.GetIssue(ctx, childID)
		if err != nil {
			continue
		}
		parent, err := h.Queries.GetIssue(ctx, parentID)
		if err != nil {
			continue
		}
		// A human who already answered is the authority; don't route around them.
		var humanAfter bool
		_ = h.DB.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM comment c
    WHERE c.issue_id = $1 AND c.deleted_at IS NULL AND c.author_type = 'member'
      AND c.created_at > (SELECT MAX(created_at) FROM comment WHERE issue_id = $1 AND deleted_at IS NULL)
)`, child.ID).Scan(&humanAfter)
		if humanAfter {
			continue
		}

		question := summariseEscalation(e.lastBody)
		commentID := h.postSweepComment(ctx, child, fmt.Sprintf(
			"升级到父级：本子任务已停 %d 分钟，最后一条是执行角色的结论，决定权在父级负责人。\n"+
				"待裁决：%s\n"+
				"父级负责人请**在本 issue** 直接给出结论（扩大写集 / 调整顺序 / 降级 / 关闭并记录原因），或明确写 `⛔ 需要人工审批 <红线 + 影响 + 默认建议>`。只在父 issue 讨论、不回到本 issue，子任务会一直停在这里。\n"+
				"<!-- escalate-up:%s -->", e.ageSecond/60, question, e.childID))
		if commentID == nil {
			continue
		}
		queued := false
		if parent.AssigneeType.Valid && parent.AssigneeID.Valid {
			switch parent.AssigneeType.String {
			case "agent":
				if _, err := h.TaskService.EnqueueTaskForMention(ctx, child, parent.AssigneeID, *commentID, service.OriginDerived); err == nil {
					queued = true
				} else {
					slog.Warn("stall sweeper: escalation enqueue failed", "issue_id", e.childID, "error", err)
				}
			case "squad":
				if sq, err := h.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{
					ID:          parent.AssigneeID,
					WorkspaceID: parent.WorkspaceID,
				}); err == nil {
					if _, err := h.TaskService.EnqueueTaskForSquadLeader(ctx, child, sq.LeaderID, sq.ID, *commentID, service.OriginDerived); err == nil {
						queued = true
					}
				}
			}
		}
		if queued {
			raised++
		}
	}
	return raised
}

// summariseEscalation keeps the child's own question, bounded, so the leader
// reads the actual blocker instead of a paraphrase of it.
func summariseEscalation(body string) string {
	trimmed := strings.TrimSpace(strings.ReplaceAll(body, "\n", " "))
	for _, prefix := range legalParkPrefixes {
		trimmed = strings.TrimPrefix(trimmed, prefix)
	}
	// "⏳ 等待确认：…" leaves "确认：…" behind; the leader should read the
	// question, not the marker's tail.
	trimmed = strings.TrimSpace(trimmed)
	for _, word := range []string{"确认", "审批", "人工"} {
		for strings.HasPrefix(trimmed, word) {
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, word))
		}
	}
	trimmed = strings.TrimSpace(strings.TrimLeft(trimmed, "：: -—"))
	if runes := []rune(trimmed); len(runes) > 400 {
		return string(runes[:400]) + "…"
	}
	return trimmed
}
