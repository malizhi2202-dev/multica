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

// Liveness: one invariant, one decision table, one ladder.
//
// Every non-terminal issue must be in exactly one of these states:
//
//	(a) a run is queued or running,
//	(b) it waits on unfinished children (a parent doing its job),
//	(c) it is `backlog` (not started on purpose),
//	(d) it waits on a human, sanctioned by a wait marker,
//	(e) it is `in_review` with an acceptance gate that is still current.
//
// Anything else is STALLED: nobody is working and nobody was asked. There is no
// useful "why" beyond that — the repair is always the same: wake somebody who
// can act, and if they do not, climb one rung. That is why this file has a
// single pass instead of a sweep per symptom.
//
// The ladder is explicit and monotonic, recorded in the issue's own thread:
//
//	rung 0  the issue's assignee      — "this turn ended with nobody to continue it"
//	rung 1  the parent issue's leader — "out of the child's scope" (or, with no
//	                                    parent, the workspace owner)
//	rung 2  the workspace owner       — "the leader did not rule"
//	stop    nothing further is mechanical; the last rung stays the answer.
//
// legalParkPrefixes are the only comment openings that legitimately leave an
// issue waiting on a human. Anything else that merely starts with ⏳/⛔ is agent
// shorthand for "this round failed" or "the gate rejected me" and must NOT
// excuse the issue from liveness — that is how a fleet of issues froze while
// every inspection round reported "no anomalies".
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

// addressedTo reports whether a comment asked anyone to do anything. A mention
// is the only mechanical evidence of that.
func addressedTo(content string) bool {
	return strings.Contains(content, "mention://")
}

// addressedToMember reports whether the addressee is a human. A human already
// named in the park is the end of the ladder, not a reason to climb it.
func addressedToMember(content string) bool {
	return strings.Contains(content, "mention://member/")
}

// livenessSnapshot is one issue's observable liveness state. Everything the
// decision table needs is here, so the table has no queries of its own.
//
// Classification reads the last AGENT comment, not simply the last comment: a
// system note (a child-result handoff, a gate notice) is not turn state, and
// treating it as one made a parent look "already asked". Our own liveness
// markers are tracked separately, because those DO mean an actor was woken.
type livenessSnapshot struct {
	ID            string
	Status        string
	OpenKids      int
	UnstartedKids int
	// AgentBody/AgentAt are the issue's last agent-authored comment.
	AgentBody string
	AgentAt   time.Time
	// MarkerAt is the newest liveness marker we wrote (rung/owner-asked).
	MarkerAt time.Time
	// MemberAfterAgent is true when a human spoke after that agent comment:
	// they are the actor now, and their comment wakes the assignee itself.
	MemberAfterAgent bool
	Rung             int
	GateAt           time.Time
}

// livenessAction is what the table decides for one snapshot.
type livenessAction int

const (
	actNone livenessAction = iota
	// actAccept: in_review with a current gate; the acceptance sweep closes it.
	actAccept
	// actWakeAssignee: the issue stopped with no successor; ask its own agent.
	actWakeAssignee
	// actCloseReview: a review cannot close itself; ask the assignee to finish.
	actCloseReview
	// actWakeParent: a parent's only open child never started; ask the parent.
	actWakeParent
	// actClimb: the previous rung did not act; hand it one rung up.
	actClimb
)

// classifyLiveness is the whole decision table. Pure: no I/O, no clock.
//
// idle is time since the last comment. The rules in order:
//
//  1. A current gate means the work is accepted; closing it is the acceptance
//     sweep's job, not this one's.
//  2. A parent whose last open child never started is waiting on something
//     nobody will start — that parent gets woken.
//  3. A review whose gate is missing or superseded cannot close itself.
//  4. A sanctioned wait that was addressed to someone is a real wait; once it
//     goes stale it climbs, because the addressed party did not act.
//  5. A turn that ended without addressing anyone and without a sanction is a
//     dead end.
//  6. Anything still busy or recent enough is left alone.
func classifyLiveness(s livenessSnapshot, now time.Time, pol LivenessPolicy) livenessAction {
	// A human spoke after the agent: they are the actor now, and their comment
	// wakes the assignee itself. Nothing mechanical to do.
	if s.MemberAfterAgent {
		return actNone
	}
	// A current gate means the work is accepted; closing it is acceptance's job.
	if s.Status == "in_review" && !s.GateAt.IsZero() && !s.GateAt.Before(s.AgentAt) {
		return actAccept
	}
	if s.OpenKids > 0 {
		if s.OpenKids == 1 && s.UnstartedKids == 1 && now.Sub(s.AgentAt) >= pol.AddressedIdle {
			return actWakeParent
		}
		// Waiting on children that are running is exactly what a parent should
		// be doing; the barrier will wake it.
		return actNone
	}
	// We already nudged after this turn: give that nudge its window, then climb.
	// The marker is the only system comment that means "an actor was asked".
	if !s.MarkerAt.IsZero() && s.MarkerAt.After(s.AgentAt) {
		if now.Sub(s.MarkerAt) < pol.AddressedIdle {
			return actNone
		}
		return actClimb
	}
	if s.Status == "in_review" {
		if now.Sub(s.AgentAt) < pol.ReviewIdle {
			return actNone
		}
		return actCloseReview
	}

	parked := isLegalParkComment(s.AgentBody)
	if parked {
		if addressedToMember(s.AgentBody) {
			// The person who must decide was named in the park itself; the
			// unanswered-human-request pass makes sure they are told.
			return actNone
		}
		if !addressedTo(s.AgentBody) {
			// A park that named nobody: nobody is coming. Climb immediately.
			return actClimb
		}
		if now.Sub(s.AgentAt) < pol.WaitIdle {
			return actNone
		}
		return actClimb
	}

	if addressedTo(s.AgentBody) {
		if now.Sub(s.AgentAt) < pol.AddressedIdle {
			return actNone
		}
		return actWakeAssignee
	}
	if now.Sub(s.AgentAt) < pol.DeadEndIdle {
		return actNone
	}
	return actWakeAssignee
}

// Rung numbers record WHO was last asked, so the ladder never asks the same
// actor twice and never skips a level:
//
//	1  the issue's own assignee (a person, an agent, or a squad's leader)
//	2  the parent issue's leader
//	3  the workspace owner
//
// climbTarget is the next rung up. A level that was already asked and did not
// act is skipped rather than asked again — that is what used to turn "the squad
// was woken" into an infinite wake loop.
func climbTarget(current int, hasParent bool) (next int, stop bool) {
	if current >= 3 {
		return current, true
	}
	if hasParent && current < 2 {
		return 2, false
	}
	return 3, false
}

// rungMarker records the ladder position in the thread so restarts and later
// ticks agree on where the issue stands.
func rungMarker(rung int, issueID string) string {
	return fmt.Sprintf("<!-- rung:%d:%s -->", rung, issueID)
}

// livenessIssuesSQL is the single candidate query: every non-terminal issue with
// no run in flight and no unfinished children. Classification happens in Go.
const livenessIssuesSQL = `
WITH active AS (
    SELECT DISTINCT issue_id
    FROM agent_task_queue
    WHERE status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
), kids AS (
    SELECT parent_issue_id,
           COUNT(*) FILTER (WHERE status NOT IN ('done', 'cancelled')) AS open_kids,
           COUNT(*) FILTER (WHERE status IN ('backlog', 'todo')) AS unstarted_kids
    FROM issue
    WHERE parent_issue_id IS NOT NULL
    GROUP BY parent_issue_id
), last_agent AS (
    SELECT DISTINCT ON (issue_id) issue_id, content, created_at
    FROM comment
    WHERE deleted_at IS NULL AND author_type = 'agent'
    ORDER BY issue_id, created_at DESC
), last_marker AS (
    SELECT issue_id, MAX(created_at) AS at
    FROM comment
    WHERE deleted_at IS NULL
      AND (content LIKE '%<!-- rung:%' OR content LIKE '%<!-- owner-asked:%')
    GROUP BY issue_id
), member_after AS (
    SELECT DISTINCT a.issue_id
    FROM last_agent a
    JOIN comment m ON m.issue_id = a.issue_id
    WHERE m.deleted_at IS NULL AND m.author_type = 'member' AND m.created_at > a.created_at
), rung AS (
    SELECT issue_id, MAX((regexp_match(content, '<!-- rung:([0-9]+):'))[1]::int) AS n
    FROM comment
    WHERE deleted_at IS NULL AND content LIKE '%<!-- rung:%'
    GROUP BY issue_id
), gate AS (
    SELECT issue_id, MAX(created_at) AS at
    FROM comment
    WHERE deleted_at IS NULL AND content ~ '(^|[\n])✅ 验收门[:：]'
    GROUP BY issue_id
)
SELECT i.id::text, i.status,
       COALESCE(k.open_kids, 0), COALESCE(k.unstarted_kids, 0),
       a.content, a.created_at,
       COALESCE(mk.at, 'epoch'::timestamptz),
       (ma.issue_id IS NOT NULL),
       COALESCE(r.n, 0),
       COALESCE(g.at, 'epoch'::timestamptz)
FROM issue i
JOIN last_agent a ON a.issue_id = i.id
LEFT JOIN active ac ON ac.issue_id = i.id
LEFT JOIN kids k ON k.parent_issue_id = i.id
LEFT JOIN last_marker mk ON mk.issue_id = i.id
LEFT JOIN member_after ma ON ma.issue_id = i.id
LEFT JOIN rung r ON r.issue_id = i.id
LEFT JOIN gate g ON g.issue_id = i.id
WHERE ac.issue_id IS NULL
  AND i.status IN ('in_progress', 'blocked', 'in_review')
  AND i.assignee_type IS NOT NULL AND i.assignee_id IS NOT NULL
  -- Parents normally wait on children; that is their job. The one exception is a
  -- parent whose only open child never started: nothing will ever make progress.
  AND (COALESCE(k.open_kids, 0) = 0
       OR (k.open_kids = 1 AND k.unstarted_kids = 1))
ORDER BY a.created_at
LIMIT 200
`

// applyLadder folds the ladder into any repair. A repair that would ask the same
// actor twice is not a repair: once the assignee has been asked (rung 1) and the
// issue stalled again, the next move is up a level. For a top-level issue that
// level is the workspace owner — which is where a human decision belongs.
func applyLadder(action livenessAction, rung int) livenessAction {
	if rung >= 1 && (action == actWakeAssignee || action == actCloseReview) {
		return actClimb
	}
	return action
}

// SweepLiveness is the mechanical half of delivery: it does not judge output, it
// refuses to let an issue sit with nobody working on it and nobody asked.
//
// It runs once at startup (restart recovery) and on a ticker. Each action is
// best-effort: one bad row never blocks the rest.
func (h *Handler) SweepLiveness(ctx context.Context) int {
	pol := h.livenessPolicy()
	h.reconcileContradictoryStatuses(ctx)
	// Independent of the stall table: a pending human decision must reach a human
	// even while agents keep working around it — unless the protocol already
	// decides it, which is the case for a repeated environment/credential park.
	h.sweepEnvironmentDefaultRuling(ctx)
	h.sweepUnansweredHumanRequests(ctx)

	rows, err := h.DB.Query(ctx, livenessIssuesSQL)
	if err != nil {
		slog.Warn("liveness: candidate query failed", "error", err)
		return 0
	}
	var snaps []livenessSnapshot
	for rows.Next() {
		var s livenessSnapshot
		if err := rows.Scan(&s.ID, &s.Status, &s.OpenKids, &s.UnstartedKids,
			&s.AgentBody, &s.AgentAt, &s.MarkerAt, &s.MemberAfterAgent, &s.Rung, &s.GateAt); err != nil {
			slog.Warn("liveness: scan failed", "error", err)
			continue
		}
		snaps = append(snaps, s)
	}
	rows.Close()

	now := time.Now()
	acted := 0
	for _, s := range snaps {
		if acted >= pol.Batch {
			break
		}
		switch applyLadder(classifyLiveness(s, now, pol), s.Rung) {
		case actAccept, actNone:
			continue
		case actWakeAssignee:
			if h.wakeAssignee(ctx, s) {
				acted++
			}
		case actCloseReview:
			if h.closeReview(ctx, s) {
				acted++
			}
		case actWakeParent:
			if h.wakeParentOnUnstartedChild(ctx, s) {
				acted++
			}
		case actClimb:
			if h.climb(ctx, s) {
				acted++
			}
		}
	}
	if acted > 0 {
		slog.Info("liveness: acted on stalled issues", "count", acted)
	}
	return acted
}

// reconcileContradictoryStatuses fixes a `blocked` issue that has a run in
// flight: the status and the facts cannot both be true, and a blocked issue is
// invisible to every retry path.
func (h *Handler) reconcileContradictoryStatuses(ctx context.Context) {
	rows, err := h.DB.Query(ctx, `
WITH active AS (
    SELECT DISTINCT issue_id FROM agent_task_queue
    WHERE status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
)
SELECT i.id::text FROM issue i JOIN active a ON a.issue_id = i.id
WHERE i.status = 'blocked' LIMIT 50`)
	if err != nil {
		slog.Warn("liveness: status reconcile query failed", "error", err)
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
			slog.Warn("liveness: unblock failed", "issue_id", raw, "error", err)
		}
	}
}

// wakeAssignee asks the issue's own agent to continue, with the fork it must
// pick from. This is rung 0.
func (h *Handler) wakeAssignee(ctx context.Context, s livenessSnapshot) bool {
	issue, _, ok := h.loadForAction(ctx, s)
	if !ok {
		return false
	}
	mention := h.issueAssigneeMention(ctx, issue)
	minutes := int(time.Since(s.AgentAt).Minutes())
	if minutes < 1 {
		minutes = 1
	}
	content := fmt.Sprintf(
		"%s停滞自检：本 issue 已 %d 分钟没有运行任务，仍为 `%s`（服务端机械巡检，不判断对错）。\n"+
			"本轮必须让某个人接住，四选一：\n"+
			"- **已经做完** → 发 `✅ 验收门：AC n/n；TEST/UAT 已附；diff 未越界` 并转 `in_review`。\n"+
			"- **自己能做** → 直接做：缺环境自建本地替代并标注「本地替代环境」；缺上游产物先 `git merge <上游交付分支>`；写集内先交付能交的部分。\n"+
			"- **超出本任务范围**（扩充写集、改跨任务顺序、动他人产物、动红线）→ `[@名字](mention://agent/<id>)` 派给父级负责人，并写明要他裁决什么。\n"+
			"- **要人等** → `⏳ 等待确认 <问题 + 默认建议 + 影响>`；要审批 → `⛔ 需要人工审批 <红线 + 影响 + 默认建议>`。\n"+
			"只写结论、不派发、不写标记 = 本轮没人接手。\n%s",
		mention, minutes, issue.Status, rungMarker(1, s.ID))
	if _, err := h.postSystemComment(ctx, issue, content); err != nil {
		return false
	}
	if _, err := h.TaskService.RerunIssue(ctx, issue.ID, pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{},
		func(db.Agent) bool { return true }); err != nil {
		slog.Warn("liveness: assignee rerun failed", "issue_id", s.ID, "error", err)
		return false
	}
	return true
}

// closeReview asks the assignee to finish the review: post a current gate, or
// reopen it with a reason.
func (h *Handler) closeReview(ctx context.Context, s livenessSnapshot) bool {
	issue, _, ok := h.loadForAction(ctx, s)
	if !ok {
		return false
	}
	mention := h.issueAssigneeMention(ctx, issue)
	reason := "本 issue 处于 `in_review` 但缺少 `✅ 验收门` 记录"
	if !s.GateAt.IsZero() && s.GateAt.Before(s.AgentAt) {
		reason = "`✅ 验收门` 之后又出现了智能体评论，旧门禁变成历史记录，自动收口不会再依据它"
	}
	content := fmt.Sprintf(
		"%s验收门待补：%s（已 %d 分钟无运行任务，服务端机械巡检）。\n"+
			"二选一：已满足 → 重新发一条 `✅ 验收门：AC n/n；TEST/UAT 已附；diff 未越界` 作为本 issue 的**最后一条**评论；未满足 → 置回 `in_progress` 继续，或写 `⏳ 等待确认 <问题>` / `⛔ 需要人工审批 <红线>`。\n%s",
		mention, reason, int(time.Since(s.AgentAt).Minutes()), rungMarker(1, s.ID))
	if _, err := h.postSystemComment(ctx, issue, content); err != nil {
		return false
	}
	if _, err := h.TaskService.RerunIssue(ctx, issue.ID, pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{},
		func(db.Agent) bool { return true }); err != nil {
		slog.Warn("liveness: review rerun failed", "issue_id", s.ID, "error", err)
		return false
	}
	return true
}

// wakeParentOnUnstartedChild wakes the parent whose only open child never ran:
// only done/cancelled close a stage, so this waits forever otherwise.
func (h *Handler) wakeParentOnUnstartedChild(ctx context.Context, s livenessSnapshot) bool {
	issue, _, ok := h.loadForAction(ctx, s)
	if !ok {
		return false
	}
	childLabel := h.openUnstartedChildLabel(ctx, issue)
	if childLabel == "" {
		return false
	}
	mention := h.issueAssigneeMention(ctx, issue)
	content := fmt.Sprintf(
		"%s子任务未启动：本 issue 只剩 %s 未终结，而它处于 `backlog`/`todo` 且从未运行过，已 %d 分钟没有运行任务（服务端机械巡检）。\n"+
			"请本轮决定：置 `in_progress` 开工；或置 `cancelled` 并写明原因（可重建）；确实要人等就写 `⏳ 等待确认 <问题>`。\n%s",
		mention, childLabel, int(time.Since(s.AgentAt).Minutes()), rungMarker(1, s.ID))
	if _, err := h.postSystemComment(ctx, issue, content); err != nil {
		return false
	}
	if _, err := h.TaskService.RerunIssue(ctx, issue.ID, pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{},
		func(db.Agent) bool { return true }); err != nil {
		slog.Warn("liveness: parent rerun failed", "issue_id", s.ID, "error", err)
		return false
	}
	return true
}

// climb moves the issue one rung: parent leader, then the workspace owner.
func (h *Handler) climb(ctx context.Context, s livenessSnapshot) bool {
	issue, _, ok := h.loadForAction(ctx, s)
	if !ok {
		return false
	}
	next, stop := climbTarget(s.Rung, issue.ParentIssueID.Valid)
	if stop {
		// At the top rung the only remaining mechanical act is making sure the
		// owner was actually told. An old rung-3 marker from a previous build (or
		// a ladder that stopped before notifying) must not end in silence.
		next = 3
	}
	mention := h.issueAssigneeMention(ctx, issue)
	question := summariseWait(s.AgentBody)
	minutes := int(time.Since(s.AgentAt).Minutes())
	if minutes < 1 {
		minutes = 1
	}

	if next == 1 {
		parent, err := h.Queries.GetIssue(ctx, issue.ParentIssueID)
		if err != nil {
			return false
		}
		leaderID, squadID, ok := h.parentLeader(ctx, parent)
		if !ok {
			return false
		}
		content := fmt.Sprintf(
			"%s升级到父级：本子任务已停 %d 分钟，最后一条是执行角色的结论，决定权在父级负责人。\n"+
				"待裁决：%s\n"+
				"父级负责人请**在本 issue** 给出结论（扩写集 / 调整顺序 / 降级 / 关闭并记录原因），或写明 `⛔ 需要人工审批 <红线 + 影响 + 默认建议>`。只在父 issue 讨论、不回到本 issue，子任务会一直停在这里。\n%s",
			mention, minutes, question, rungMarker(2, s.ID))
		commentID, err := h.postSystemComment(ctx, issue, content)
		if err != nil {
			return false
		}
		return h.enqueueLeader(ctx, issue, leaderID, squadID, commentID)
	}

	ownerID, ownerName, ok := h.workspaceOwner(ctx, issue.WorkspaceID)
	if !ok {
		return false
	}
	// One owner notification per issue per stretch: the unanswered-human-request
	// pass already tells the owner about a pending decision, and two messages for
	// the same thing is exactly the noise this consolidation removed.
	var toldRecently bool
	if err := h.DB.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM comment
    WHERE issue_id = $1 AND deleted_at IS NULL
      AND content LIKE '%<!-- owner-asked:%'
      AND created_at > now() - interval '6 hours'
)`, issue.ID).Scan(&toldRecently); err == nil && toldRecently {
		if _, err := h.postSystemComment(ctx, issue, fmt.Sprintf(
			"阶梯到顶：本 issue 已把待人工确认的请求送到工作区所有者（6 小时内不重复打扰），机械兜底到此为止。\n%s",
			rungMarker(3, s.ID))); err == nil {
			slog.Info("liveness: owner already told; ladder closed", "issue_id", s.ID)
		}
		return false
	}
	escalatedFrom := "本任务的执行角色"
	if issue.ParentIssueID.Valid {
		if parent, err := h.Queries.GetIssue(ctx, issue.ParentIssueID); err == nil {
			escalatedFrom = fmt.Sprintf("父 issue [#%d %s](mention://issue/%s) 的负责人",
				parent.Number, parent.Title, uuidToString(parent.ID))
		}
	}
	content := fmt.Sprintf(
		"[@%s](mention://member/%s) 需要你定一次：本 issue [#%d %s](mention://issue/%s) 机械兜底已到最后一档。\n"+
			"升级来源：%s（已 %d 分钟没有裁决）。现状：`%s`，无运行任务；待裁决：%s\n"+
			"请二选一：① 给一句裁决（谁做、做什么、做到什么算完成）→ 我继续；② 判定不值得继续 → 把 issue 置 `cancelled` 或改写验收口径。\n"+
			"默认建议：若只是缺上游产物或授权，按现有写集降级交付并在证据标注缺口，不要继续空转。\n<!-- owner-asked:ladder:%s -->\n%s",
		ownerName, uuidToString(ownerID), issue.Number, issue.Title, uuidToString(issue.ID),
		escalatedFrom, minutes, issue.Status, question, s.ID, rungMarker(3, s.ID))
	if _, err := h.postSystemComment(ctx, issue, content); err != nil {
		return false
	}
	return true
}

// loadForAction re-reads the issue and re-checks that it is still non-terminal
// and still has no run: the snapshot the table classified may be stale.
func (h *Handler) loadForAction(ctx context.Context, s livenessSnapshot) (db.Issue, string, bool) {
	id, err := util.ParseUUID(s.ID)
	if err != nil {
		return db.Issue{}, "", false
	}
	issue, err := h.Queries.GetIssue(ctx, id)
	if err != nil {
		return db.Issue{}, "", false
	}
	switch issue.Status {
	case "in_progress", "blocked", "in_review":
	default:
		return db.Issue{}, "", false
	}
	var running int
	if err := h.DB.QueryRow(ctx, `
SELECT COUNT(*) FROM agent_task_queue
WHERE issue_id = $1 AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')`,
		issue.ID).Scan(&running); err != nil || running > 0 {
		return db.Issue{}, "", false
	}
	return issue, s.AgentBody, true
}

// postSystemComment writes one system comment and returns its id.
func (h *Handler) postSystemComment(ctx context.Context, issue db.Issue, content string) (pgtype.UUID, error) {
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
		slog.Warn("liveness: comment failed", "issue_id", uuidToString(issue.ID), "error", err)
		return pgtype.UUID{}, err
	}
	return id, nil
}

// enqueueLeader queues the parent's decision-maker on the child issue.
func (h *Handler) enqueueLeader(ctx context.Context, issue db.Issue, leaderID, squadID, commentID pgtype.UUID) bool {
	if squadID.Valid {
		if _, err := h.TaskService.EnqueueTaskForSquadLeader(ctx, issue, leaderID, squadID, commentID, service.OriginDerived); err == nil {
			return true
		} else {
			slog.Warn("liveness: squad leader enqueue failed", "issue_id", uuidToString(issue.ID), "error", err)
		}
	}
	if _, err := h.TaskService.EnqueueTaskForMention(ctx, issue, leaderID, commentID, service.OriginDerived); err != nil {
		slog.Warn("liveness: leader enqueue failed", "issue_id", uuidToString(issue.ID), "error", err)
		return false
	}
	return true
}

// parentLeader resolves a parent issue's decision-maker.
func (h *Handler) parentLeader(ctx context.Context, parent db.Issue) (pgtype.UUID, pgtype.UUID, bool) {
	if !parent.AssigneeType.Valid || !parent.AssigneeID.Valid {
		return pgtype.UUID{}, pgtype.UUID{}, false
	}
	switch parent.AssigneeType.String {
	case "agent":
		return parent.AssigneeID, pgtype.UUID{}, true
	case "squad":
		sq, err := h.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{
			ID:          parent.AssigneeID,
			WorkspaceID: parent.WorkspaceID,
		})
		if err != nil {
			return pgtype.UUID{}, pgtype.UUID{}, false
		}
		return sq.LeaderID, sq.ID, true
	}
	return pgtype.UUID{}, pgtype.UUID{}, false
}

// workspaceOwner resolves the human of last resort for a workspace.
func (h *Handler) workspaceOwner(ctx context.Context, workspaceID pgtype.UUID) (pgtype.UUID, string, bool) {
	var ownerID pgtype.UUID
	if err := h.DB.QueryRow(ctx,
		`SELECT user_id FROM member WHERE workspace_id = $1 AND role = 'owner' LIMIT 1`,
		workspaceID).Scan(&ownerID); err != nil {
		return pgtype.UUID{}, "", false
	}
	var name string
	if err := h.DB.QueryRow(ctx, `SELECT name FROM "user" WHERE id = $1`, ownerID).Scan(&name); err != nil || name == "" {
		name = "工作空间所有者"
	}
	return ownerID, name, true
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

// openUnstartedChildLabel renders the single open never-ran child, or "" when the
// shape changed since the snapshot was taken.
func (h *Handler) openUnstartedChildLabel(ctx context.Context, parent db.Issue) string {
	rows, err := h.DB.Query(ctx,
		`SELECT id::text, number, title FROM issue
		 WHERE parent_issue_id = $1 AND status NOT IN ('done', 'cancelled')
		 ORDER BY number`, parent.ID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var id, title string
	var number int32
	count := 0
	for rows.Next() {
		if err := rows.Scan(&id, &number, &title); err != nil {
			return ""
		}
		count++
	}
	if err := rows.Err(); err != nil || count != 1 {
		return ""
	}
	var started bool
	if err := h.DB.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM agent_task_queue WHERE issue_id = $1)`, id).Scan(&started); err != nil {
		return ""
	}
	if started {
		return ""
	}
	return fmt.Sprintf("[#%d %s](mention://issue/%s)", number, title, id)
}

// summariseWait keeps the parked issue's own question, bounded, so the next rung
// reads the actual blocker instead of a paraphrase of it.
func summariseWait(body string) string {
	trimmed := strings.TrimSpace(strings.ReplaceAll(body, "\n", " "))
	for _, prefix := range legalParkPrefixes {
		trimmed = strings.TrimPrefix(trimmed, prefix)
	}
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

// unansweredHumanRequestsSQL finds an agent's sanctioned wait marker that a human
// never answered and the owner was never told about.
//
// This is NOT the same question as "is the issue stalled". The agents can keep
// working around a pending human ruling — and did, for an hour on an issue whose
// own thread said "等待确认…待发起人裁决" while every sweep woke an agent instead
// of telling the one person who could unblock it. A wait marker is a request to a
// person; if no person replied, the request must reach a person.
const unansweredHumanRequestsSQL = `
WITH agent_park AS (
    SELECT DISTINCT ON (issue_id) issue_id, id, content, created_at
    FROM comment
    WHERE deleted_at IS NULL
      AND author_type = 'agent'
      AND (content LIKE '⏳%' OR content LIKE '⛔%')
    ORDER BY issue_id, created_at DESC
), answered AS (
    SELECT p.issue_id
    FROM agent_park p
    JOIN comment m ON m.issue_id = p.issue_id
    WHERE m.deleted_at IS NULL AND m.author_type = 'member' AND m.created_at > p.created_at
    GROUP BY p.issue_id
), told AS (
    SELECT issue_id, MAX(created_at) AS at
    FROM comment
    WHERE deleted_at IS NULL AND content LIKE '%<!-- owner-asked:%'
    GROUP BY issue_id
)
SELECT p.issue_id::text, p.id::text, p.content, p.created_at
FROM agent_park p
JOIN issue i ON i.id = p.issue_id
LEFT JOIN answered a ON a.issue_id = p.issue_id
LEFT JOIN told t ON t.issue_id = p.issue_id
WHERE a.issue_id IS NULL
  AND i.status IN ('in_progress', 'blocked', 'in_review')
  AND p.created_at < now() - make_interval(secs => $1)
  AND (t.at IS NULL OR t.at < p.created_at)
  AND p.content NOT LIKE '%mention://agent/%'
  AND p.content NOT LIKE '%mention://squad/%'
ORDER BY p.created_at
LIMIT $2
`

// sweepUnansweredHumanRequests surfaces a pending human decision to its owner.
func (h *Handler) sweepUnansweredHumanRequests(ctx context.Context) int {
	pol := h.livenessPolicy()
	rows, err := h.DB.Query(ctx, unansweredHumanRequestsSQL,
		int(pol.HumanRequestGrace.Seconds()), pol.Batch)
	if err != nil {
		slog.Warn("liveness: human request query failed", "error", err)
		return 0
	}
	type request struct {
		issueID  string
		parkID   string
		body     string
		parkedAt time.Time
	}
	var list []request
	for rows.Next() {
		var r request
		if err := rows.Scan(&r.issueID, &r.parkID, &r.body, &r.parkedAt); err != nil {
			continue
		}
		list = append(list, r)
	}
	rows.Close()

	told := 0
	for _, r := range list {
		id, err := util.ParseUUID(r.issueID)
		if err != nil {
			continue
		}
		issue, err := h.Queries.GetIssue(ctx, id)
		if err != nil {
			continue
		}
		ownerID, ownerName, ok := h.workspaceOwner(ctx, issue.WorkspaceID)
		if !ok {
			continue
		}
		minutes := int(time.Since(r.parkedAt).Minutes())
		if minutes < 1 {
			minutes = 1
		}
		parentText := ""
		if issue.ParentIssueID.Valid {
			if parent, err := h.Queries.GetIssue(ctx, issue.ParentIssueID); err == nil {
				parentText = fmt.Sprintf("（父 issue [#%d %s](mention://issue/%s)）",
					parent.Number, parent.Title, uuidToString(parent.ID))
			}
		}
		if _, err := h.postSystemComment(ctx, issue, fmt.Sprintf(
			"[@%s](mention://member/%s) 有一条等待人工确认已经 %d 分钟没人回：本 issue [#%d %s](mention://issue/%s)%s\n"+
				"待确认：%s\n"+
				"两条出路：① 给一句结论（做/不做、按哪个口径）→ 我继续推进；② 判定不值得继续 → 置 `cancelled` 或改写验收口径。\n"+
				"默认建议：若只是为了等一个偏好或授权，选一个默认口径继续，把假设写进验收证据，不要空等。\n"+
				"<!-- owner-asked:%s -->",
			ownerName, uuidToString(ownerID), minutes, issue.Number, issue.Title,
			uuidToString(issue.ID), parentText, summariseWait(r.body), r.parkID)); err != nil {
			continue
		}
		told++
		slog.Info("liveness: surfaced an unanswered human request to the owner",
			"issue_id", r.issueID, "park_comment", r.parkID)
	}
	return told
}

// environmentParksSQL finds an issue that keeps parking on an environment,
// credential or authorisation gap while a local substitute is perfectly usable.
//
// The rule "环境缺失 ≠ 阻塞" has been in the protocol for a while, yet an issue
// asked for "a registered MySQL / five remote identities / an authorisation
// original" round after round, each time escalating to a human. Asking again is
// not a decision: after the second such park the system records the default
// ruling itself and hands the issue back, because the substitute path is
// already defined and does not need a person.
const environmentParksSQL = `
WITH agent_comments AS (
    SELECT issue_id, id, content, created_at
    FROM comment
    WHERE deleted_at IS NULL AND author_type = 'agent'
), newest_agent AS (
    SELECT DISTINCT ON (issue_id) issue_id, id, content, created_at
    FROM agent_comments
    ORDER BY issue_id, created_at DESC
), env AS (
    SELECT issue_id, COUNT(*) AS env_parks
    FROM agent_comments
    WHERE content ~* '(环境|数据库|mysql|schema|凭据|凭证|身份|授权|签发|登记|维护方|secret|token|权限|not configured|未经授权|连接)'
      AND content !~* '(生产|线上|prod|删除|发布|部署|外发|sudo)'
      AND created_at > now() - interval '24 hours'
    GROUP BY issue_id
), answered AS (
    SELECT DISTINCT n.issue_id
    FROM newest_agent n
    JOIN comment m ON m.issue_id = n.issue_id
    WHERE m.deleted_at IS NULL AND m.author_type = 'member' AND m.created_at > n.created_at
), ruled AS (
    SELECT issue_id, MAX(created_at) AS at
    FROM comment
    WHERE deleted_at IS NULL AND content LIKE '%<!-- default-ruling:env:%'
    GROUP BY issue_id
)
SELECT n.issue_id::text, n.content, n.created_at, e.env_parks
FROM newest_agent n
JOIN env e ON e.issue_id = n.issue_id
JOIN issue i ON i.id = n.issue_id
LEFT JOIN answered a ON a.issue_id = n.issue_id
LEFT JOIN ruled r ON r.issue_id = n.issue_id
WHERE a.issue_id IS NULL
  AND i.status IN ('in_progress', 'blocked')
  AND e.env_parks >= 3
  AND n.content ~* '(环境|数据库|mysql|schema|凭据|凭证|身份|授权|签发|登记|维护方|secret|token|权限|not configured|未经授权|连接)'
  AND n.content !~* '(生产|线上|prod|删除|发布|部署|外发|sudo)'
  AND n.created_at < now() - make_interval(secs => $1)
  AND (r.at IS NULL OR r.at < n.created_at)
ORDER BY n.created_at
LIMIT $2
`

// sweepEnvironmentDefaultRuling records the protocol's default ruling for a
// repeated environment/authorisation park and hands the issue back to its agent.
func (h *Handler) sweepEnvironmentDefaultRuling(ctx context.Context) int {
	pol := h.livenessPolicy()
	rows, err := h.DB.Query(ctx, environmentParksSQL,
		int(pol.HumanRequestGrace.Seconds()), pol.Batch)
	if err != nil {
		slog.Warn("liveness: environment ruling query failed", "error", err)
		return 0
	}
	type park struct {
		issueID string
		body    string
		parked  time.Time
		count   int
	}
	var list []park
	for rows.Next() {
		var p park
		if err := rows.Scan(&p.issueID, &p.body, &p.parked, &p.count); err != nil {
			continue
		}
		list = append(list, p)
	}
	rows.Close()

	ruled := 0
	for _, p := range list {
		id, err := util.ParseUUID(p.issueID)
		if err != nil {
			continue
		}
		issue, err := h.Queries.GetIssue(ctx, id)
		if err != nil {
			continue
		}
		mention := h.issueAssigneeMention(ctx, issue)
		commentID, postErr := h.postSystemComment(ctx, issue, fmt.Sprintf(
			"%s默认裁决（环境缺失 ≠ 阻塞）：本 issue 已第 %d 次以「登记环境 / 凭据 / 身份授权」为由停车，机械兜底按协议直接裁决，不再问人。\n"+
				"裁决：**按本地替代环境继续** —— 先用**本机**（`127.0.0.1` / `localhost`，含本机已开的等价端口，如 3307）的实例建库建角色，不要拿远端 IP 当唯一入口；账号/密钥按代码生成合成身份，外部系统用 mock，走通同一条验收路径。证据逐条标注「本地替代环境」，不得写成登记环境通过。\n"+
				"本轮要求：按上述方式补齐能补的证据并推进；因本地替代而缺的项，写进交付说明与验收证据，不改 AC 口径也不冒充通过。\n"+
				"仅当涉及生产 / 线上 / 凭据外发等红线时，才允许改写成 `⛔ 需要人工审批 <红线 + 影响 + 默认建议>`。\n"+
				"<!-- default-ruling:env:%s -->",
			mention, p.count, p.issueID))
		if postErr != nil {
			continue
		}
		if _, err := h.TaskService.RerunIssue(ctx, issue.ID, pgtype.UUID{}, commentID, pgtype.UUID{},
			func(db.Agent) bool { return true }); err != nil {
			slog.Warn("liveness: environment ruling rerun failed", "issue_id", p.issueID, "error", err)
			continue
		}
		ruled++
		slog.Info("liveness: recorded the default environment ruling", "issue_id", p.issueID, "parks", p.count)
	}
	return ruled
}
