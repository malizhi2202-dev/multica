package handler

import (
	"context"
	"strings"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

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
