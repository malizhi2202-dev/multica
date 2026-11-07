package handler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// A parked issue is one that legitimately waits on a human. Everything else that
// merely opens with ⏳/⛔ is work that stopped, and must stay visible to liveness.
func TestIsLegalParkComment(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"waiting for confirmation", "⏳ 等待确认：是否需要同时支持 v1 与 v2？", true},
		{"waiting for approval", "⏳ 等待审批：生产库写入需要授权", true},
		{"leading space", "  ⛔ 需要人工审批：删除线上数据库表", true},
		{"round failed", "⛔ 本轮未能在当前 checkout 实施或验收 T16", false},
		{"gate rejected", "⛔ 验收门未过：缺少 ✅ 验收门 记录", false},
		{"role boundary", "⛔ 当前提醒并非门禁邀请，本轮不投票", false},
		{"plain progress", "已完成 T16 实现并补证", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		if got := isLegalParkComment(tc.content); got != tc.want {
			t.Errorf("%s: isLegalParkComment(%q) = %v, want %v", tc.name, tc.content, got, tc.want)
		}
	}
}

// A mention is the only mechanical evidence that a comment asked someone to act.
func TestAddressedTo(t *testing.T) {
	if !addressedTo("请 [@张三](mention://agent/abc) 复核") {
		t.Error("agent mention not detected")
	}
	if !addressedTo("请 [@所有人](mention://all/abc) 注意") {
		t.Error("all mention not detected")
	}
	if addressedTo("请负责人确认") {
		t.Error("a role name is not a dispatch")
	}
}

// The ladder records who was last asked and climbs without repeating a level.
func TestClimbTarget(t *testing.T) {
	cases := []struct {
		current   int
		hasParent bool
		wantNext  int
		wantStop  bool
	}{
		{0, true, 2, false},  // park that named nobody, with a parent → parent
		{0, false, 3, false}, // no parent → owner
		{1, true, 2, false},  // assignee asked → parent
		{1, false, 3, false}, // assignee asked, no parent → owner
		{2, true, 3, false},  // parent asked → owner
		{3, true, 3, true},   // owner is the last rung
		{9, true, 9, true},   // never moves down
	}
	for _, tc := range cases {
		next, stop := climbTarget(tc.current, tc.hasParent)
		if next != tc.wantNext || stop != tc.wantStop {
			t.Errorf("climbTarget(%d, %v) = (%d, %v), want (%d, %v)",
				tc.current, tc.hasParent, next, stop, tc.wantNext, tc.wantStop)
		}
	}
}

// The decision table is the whole liveness contract; every state that is allowed
// to sit must classify as nothing-to-do, and every state that is not must have an
// action. Table-driven so a rule change shows up here first.
func TestClassifyLiveness(t *testing.T) {
	pol := DefaultLivenessPolicy()
	now := time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	gate := ago(30 * time.Minute)

	cases := []struct {
		name string
		snap livenessSnapshot
		want livenessAction
	}{
		{
			name: "recent human comment gets time to trigger assignee",
			snap: livenessSnapshot{Status: "in_progress", AgentAt: ago(3 * time.Hour),
				AgentBody: "T01 仍未通过", MemberAt: ago(time.Minute)},
			want: actNone,
		},
		{
			name: "human comment trigger failed and has no active run",
			snap: livenessSnapshot{Status: "in_progress", AgentAt: ago(3 * time.Hour),
				AgentBody: "T01 仍未通过", MemberAt: ago(time.Hour)},
			want: actWakeAssignee,
		},
		{
			name: "current gate is accepted, not our job",
			snap: livenessSnapshot{Status: "in_review", AgentAt: gate,
				AgentBody: "✅ 验收门：AC 3/3", GateAt: gate},
			want: actAccept,
		},
		{
			name: "in progress with current gate waits for barrier sweep",
			snap: livenessSnapshot{Status: "in_progress", AgentAt: gate,
				AgentBody: "✅ 验收门：AC 3/3", GateAt: gate},
			want: actAccept,
		},
		{
			name: "review with a superseded gate",
			snap: livenessSnapshot{Status: "in_review", AgentAt: ago(15 * time.Minute),
				AgentBody: "本轮未能完成", GateAt: gate},
			want: actCloseReview,
		},
		{
			name: "review too fresh to nudge",
			snap: livenessSnapshot{Status: "in_review", AgentAt: ago(time.Minute),
				AgentBody: "已置 in_review"},
			want: actNone,
		},
		{
			name: "parent waiting on a never-started child",
			snap: livenessSnapshot{Status: "in_progress", OpenKids: 1, UnstartedKids: 1,
				AgentAt: ago(time.Hour), AgentBody: "等子任务"},
			want: actWakeParent,
		},
		{
			name: "parent waiting on several never-started children",
			snap: livenessSnapshot{Status: "in_progress", OpenKids: 3, UnstartedKids: 3,
				AgentAt: ago(time.Hour), AgentBody: "等子任务"},
			want: actWakeParent,
		},
		{
			name: "parent has an unassigned child with no run",
			snap: livenessSnapshot{Status: "in_progress", OpenKids: 2, OrphanKids: 1,
				AgentAt: ago(time.Hour), AgentBody: "等子任务"},
			want: actWakeParent,
		},
		{
			name: "parent waiting on a running child is fine",
			snap: livenessSnapshot{Status: "in_progress", OpenKids: 1, UnstartedKids: 0,
				AgentAt: ago(time.Hour), AgentBody: "等子任务"},
			want: actNone,
		},
		{
			name: "our own nudge after the turn: wait, then climb",
			snap: livenessSnapshot{Status: "in_progress", AgentAt: ago(30 * time.Minute),
				AgentBody: "工程未完成", MarkerAt: ago(2 * time.Minute)},
			want: actNone,
		},
		{
			name: "nudge went stale without a reply",
			snap: livenessSnapshot{Status: "in_progress", AgentAt: ago(3 * time.Hour),
				AgentBody: "工程未完成", MarkerAt: ago(time.Hour)},
			want: actClimb,
		},
		{
			name: "park that named nobody climbs at once",
			snap: livenessSnapshot{Status: "in_progress", AgentAt: ago(time.Minute),
				AgentBody: "⏳ 等待确认：请把 write_files 扩到 .gitignore"},
			want: actClimb,
		},
		{
			name: "park addressed to someone is a real wait",
			snap: livenessSnapshot{Status: "in_progress", AgentAt: ago(5 * time.Minute),
				AgentBody: "⏳ 等待确认：请 [@负责人](mention://agent/x) 裁决写集"},
			want: actNone,
		},
		{
			name: "recent park addressed to a human remains a real wait",
			snap: livenessSnapshot{Status: "in_progress", AgentAt: ago(5 * time.Minute),
				AgentBody: "⏳ 等待确认：[@owner](mention://member/abc) 请提供审核分支"},
			want: actNone,
		},
		{
			name: "unanswered human park eventually climbs so a child can close",
			snap: livenessSnapshot{Status: "in_progress", AgentAt: ago(3 * time.Hour),
				AgentBody: "⏳ 等待确认：[@owner](mention://member/abc) 请提供审核分支"},
			want: actClimb,
		},
		{
			name: "stale addressed park climbs",
			snap: livenessSnapshot{Status: "in_progress", AgentAt: ago(time.Hour),
				AgentBody: "⏳ 等待确认：请 [@负责人](mention://agent/x) 裁决写集"},
			want: actClimb,
		},
		{
			name: "dead end climbs to the assignee within minutes",
			snap: livenessSnapshot{Status: "in_progress", AgentAt: ago(4 * time.Minute),
				AgentBody: "T01 继续 BLOCKED，W1/T02 仍锁定"},
			want: actWakeAssignee,
		},
		{
			name: "dead end too fresh",
			snap: livenessSnapshot{Status: "in_progress", AgentAt: ago(time.Minute),
				AgentBody: "T01 继续 BLOCKED"},
			want: actNone,
		},
		{
			name: "turn that named an agent gets the longer window",
			snap: livenessSnapshot{Status: "in_progress", AgentAt: ago(10 * time.Minute),
				AgentBody: "请 [@评审](mention://agent/x) 投票"},
			want: actNone,
		},
		{
			name: "named agent never took it",
			snap: livenessSnapshot{Status: "in_progress", AgentAt: ago(time.Hour),
				AgentBody: "请 [@评审](mention://agent/x) 投票"},
			want: actWakeAssignee,
		},
	}

	for _, tc := range cases {
		if got := classifyLiveness(tc.snap, now, pol); got != tc.want {
			t.Errorf("%s: classifyLiveness = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The leader must read the blocker itself: the park prefix is stripped and the
// question kept, bounded.
func TestSummariseWait(t *testing.T) {
	got := summariseWait("⏳ 等待确认：请把 T01 的 write_files 扩展为包含 evidence/.gitignore\n并授权替换取证记录")
	want := "请把 T01 的 write_files 扩展为包含 evidence/.gitignore 并授权替换取证记录"
	if got != want {
		t.Errorf("summariseWait = %q, want %q", got, want)
	}
	long := "⛔ 需要人工审批：" + strings.Repeat("长", 600)
	if out := []rune(summariseWait(long)); len(out) != 401 {
		t.Errorf("long question not bounded: %d runes", len(out))
	}
}

// The gate sweep must not close an in_progress issue whose own last word is a
// marker, and must not re-close one an agent spoke after.
func TestGateAutoAdvanceBlocked(t *testing.T) {
	cases := []struct {
		status, lastComment string
		want                bool
	}{
		{"in_progress", "⏳ 等待确认：是否支持 v1？", true},
		{"in_progress", "⛔ 需要人工审批：删除线上表", true},
		{"in_progress", "⛔ 本轮未能在当前 checkout 实施 T16", true},
		{"in_progress", "已完成 T16 实现并补证", false},
		{"in_review", "✅ 验收门：AC 3/3", false},
		{"in_review", "⛔ 验收门未过：缺少记录", true},
	}
	for _, tc := range cases {
		if got := gateAutoAdvanceBlocked(tc.status, tc.lastComment); got != tc.want {
			t.Errorf("gateAutoAdvanceBlocked(%q, %q) = %v, want %v", tc.status, tc.lastComment, got, tc.want)
		}
	}
}

func TestGateSupersededByActor(t *testing.T) {
	gate := time.Date(2026, 11, 5, 17, 17, 0, 0, time.UTC)
	if gateSupersededByActor(gate, gate) {
		t.Error("gate as the last agent word must not be superseded")
	}
	if !gateSupersededByActor(gate, gate.Add(time.Hour)) {
		t.Error("an agent speaking after the gate supersedes it")
	}
}

func TestBarrierDoesNotAcceptGateAfterMemberRequestsRework(t *testing.T) {
	ctx := context.Background()
	agentID := dbfx.Agent(t, "barrier review agent", testRuntimeID)
	issueID := dbfx.Issue(t, "review with rework request", map[string]any{"status": "in_review"})
	dbfx.Comment(t, issueID, "✅ 验收门：AC 1/1", map[string]any{
		"author_type": "agent", "author_id": agentID, "created_at": time.Now().Add(-30 * time.Minute),
	})
	dbfx.Comment(t, issueID, "请先修复 T01，再提交验收", map[string]any{
		"created_at": time.Now().Add(-20 * time.Minute),
	})
	if moved := testHandler.SweepBarrierLiveness(ctx); moved != 0 {
		t.Fatalf("barrier accepted a superseded gate, moved=%d", moved)
	}
	id, err := util.ParseUUID(issueID)
	if err != nil {
		t.Fatal(err)
	}
	issue, err := testHandler.Queries.GetIssue(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if issue.Status != "in_review" {
		t.Fatalf("issue status = %q after rework request, want in_review", issue.Status)
	}
}

func TestBarrierResolvesCustomStartedParentAndDoneChild(t *testing.T) {
	ctx := context.Background()
	dbfx.Insert(t, "issue_status", testutil.Cols{
		"workspace_id": testWorkspaceID, "key": "barrier_started", "name": "Barrier Started", "category": "started", "color": "#123456",
	})
	dbfx.Insert(t, "issue_status", testutil.Cols{
		"workspace_id": testWorkspaceID, "key": "barrier_done", "name": "Barrier Done", "category": "done", "color": "#123456",
	})
	agentID := dbfx.Agent(t, "barrier custom agent", testRuntimeID)
	parentID := dbfx.Issue(t, "custom barrier parent", map[string]any{"status": "barrier_started"})
	dbfx.Issue(t, "custom terminal child", map[string]any{"status": "barrier_done", "parent_issue_id": parentID})
	dbfx.Comment(t, parentID, "✅ 验收门：AC 1/1", map[string]any{
		"author_type": "agent", "author_id": agentID, "created_at": time.Now().Add(-30 * time.Minute),
	})
	if moved := testHandler.SweepBarrierLiveness(ctx); moved != 1 {
		t.Fatalf("custom status barrier moved=%d, want 1", moved)
	}
	id, err := util.ParseUUID(parentID)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := testHandler.Queries.GetIssue(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Status != "done" {
		t.Fatalf("custom started parent status=%q, want done", parent.Status)
	}
}

func TestBarrierRejectsMemberWrittenGate(t *testing.T) {
	ctx := context.Background()
	issueID := dbfx.Issue(t, "member quotes acceptance", map[string]any{"status": "in_review"})
	dbfx.Comment(t, issueID, "✅ 验收门：只是引用旧文本", map[string]any{
		"created_at": time.Now().Add(-30 * time.Minute),
	})
	if moved := testHandler.SweepBarrierLiveness(ctx); moved != 0 {
		t.Fatalf("member comment was accepted as agent gate, moved=%d", moved)
	}
}

func TestBarrierDoesNotCloseBlockedIssueFromHistoricalGate(t *testing.T) {
	ctx := context.Background()
	agentID := dbfx.Agent(t, "blocked gate agent", testRuntimeID)
	issueID := dbfx.Issue(t, "blocked after prior gate", map[string]any{"status": "blocked"})
	dbfx.Comment(t, issueID, "✅ 验收门：旧阶段通过", map[string]any{
		"author_type": "agent", "author_id": agentID, "created_at": time.Now().Add(-30 * time.Minute),
	})
	if moved := testHandler.SweepBarrierLiveness(ctx); moved != 0 {
		t.Fatalf("blocked issue auto closed from old gate, moved=%d", moved)
	}
}

func TestBarrierDoesNotCloseParentBeforeReviewingBackfilledChildResult(t *testing.T) {
	ctx := context.Background()
	agentID := dbfx.Agent(t, "parent review agent", testRuntimeID)
	parentID := dbfx.Issue(t, "parent gate before child result", map[string]any{
		"status": "in_review", "assignee_type": "agent", "assignee_id": agentID,
	})
	childID := dbfx.Issue(t, "child finished after parent gate", map[string]any{
		"status": "done", "parent_issue_id": parentID,
	})
	dbfx.Comment(t, parentID, "✅ 验收门：旧父级结论", map[string]any{
		"author_type": "agent", "author_id": agentID, "created_at": time.Now().Add(-40 * time.Minute),
	})
	dbfx.Comment(t, childID, "✅ 验收门：子级新结果", map[string]any{
		"author_type": "agent", "author_id": agentID, "created_at": time.Now().Add(-20 * time.Minute),
	})
	if moved := testHandler.SweepBarrierLiveness(ctx); moved != 0 {
		t.Fatalf("parent closed before reviewing new child result, moved=%d", moved)
	}
	var handoffs int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE '%<!-- handoff:%'`, parentID).Scan(&handoffs); err != nil {
		t.Fatal(err)
	}
	if handoffs != 1 {
		t.Fatalf("child result handoffs=%d, want one", handoffs)
	}
	rows, err := testPool.Query(ctx, livenessIssuesSQL)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for rows.Next() {
		var s livenessSnapshot
		if err := rows.Scan(&s.ID, &s.Status, &s.OpenKids, &s.UnstartedKids, &s.OrphanKids,
			&s.AgentBody, &s.AgentAt, &s.MarkerAt, &s.MemberAt, &s.Rung, &s.GateAt); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if s.ID == parentID {
			found = true
			if action := classifyLiveness(s, time.Now(), DefaultLivenessPolicy()); action != actCloseReview {
				rows.Close()
				t.Fatalf("parent after new child result classified as %v, want review repair", action)
			}
		}
	}
	rows.Close()
	if !found {
		t.Fatal("parent awaiting child result review absent from liveness candidates")
	}
	if _, err := testPool.Exec(ctx, `UPDATE comment SET created_at = now() - interval '20 minutes'
WHERE issue_id = $1 AND content LIKE '%<!-- handoff:%'`, parentID); err != nil {
		t.Fatal(err)
	}
	if moved := testHandler.SweepBarrierLiveness(ctx); moved != 0 {
		t.Fatalf("historical parent gate accepted after child handoff, moved=%d", moved)
	}
}

func TestBarrierStageProgressSupersedesParentGate(t *testing.T) {
	ctx := context.Background()
	agentID := dbfx.Agent(t, "stage progress review agent", testRuntimeID)
	parentID := dbfx.Issue(t, "parent awaiting stage summary", map[string]any{"status": "in_review"})
	dbfx.Comment(t, parentID, "✅ 验收门：旧父级结论", map[string]any{
		"author_type": "agent", "author_id": agentID, "created_at": time.Now().Add(-40 * time.Minute),
	})
	dbfx.Comment(t, parentID, "Stage 2 has completed", map[string]any{
		"author_type": "system", "type": "system", "created_at": time.Now().Add(-20 * time.Minute),
	})
	if moved := testHandler.SweepBarrierLiveness(ctx); moved != 0 {
		t.Fatalf("parent closed from gate before stage progress, moved=%d", moved)
	}
}

func TestBarrierRechecksCandidateAfterMemberReply(t *testing.T) {
	ctx := context.Background()
	agentID := dbfx.Agent(t, "barrier race agent", testRuntimeID)
	issueID := dbfx.Issue(t, "gate before new reply", map[string]any{"status": "in_review"})
	dbfx.Comment(t, issueID, "✅ 验收门：AC 1/1", map[string]any{
		"author_type": "agent", "author_id": agentID, "created_at": time.Now().Add(-30 * time.Minute),
	})
	var candidate, status, lastComment string
	var gateAt, actorAt time.Time
	if err := testPool.QueryRow(ctx, barrierReadyIssuesSQL,
		int(barrierSweepIdle.Seconds()), issueID).Scan(&candidate, &status, &lastComment, &gateAt, &actorAt); err != nil {
		t.Fatal(err)
	}
	dbfx.Comment(t, issueID, "验收要返工，先不要关闭")
	id, err := util.ParseUUID(issueID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, advanced := testHandler.advanceBarrierCandidate(ctx, id); advanced {
		t.Fatal("barrier advanced a candidate after a new member reply")
	}
}

// The bug that made a squad get woken again and again: the table decided "wake
// the assignee" forever because nothing folded the rung into the action. This is
// the fold.
func TestApplyLadder(t *testing.T) {
	cases := []struct {
		action livenessAction
		rung   int
		want   livenessAction
	}{
		{actWakeAssignee, 0, actWakeAssignee}, // first ask: the assignee
		{actWakeAssignee, 1, actClimb},        // already asked: climb
		{actCloseReview, 2, actClimb},         // already asked twice: still climb
		{actClimb, 0, actClimb},               // a park that named nobody
		{actWakeParent, 0, actWakeParent},     // first ask: the parent leader
		{actWakeParent, 1, actClimb},          // repeated parent wake must escalate
		{actNone, 3, actNone},
		{actAccept, 1, actAccept},
	}
	for _, tc := range cases {
		if got := applyLadder(tc.action, tc.rung); got != tc.want {
			t.Errorf("applyLadder(%v, rung %d) = %v, want %v", tc.action, tc.rung, got, tc.want)
		}
	}
}

// The thresholds must stay in the policy file only, and in the intended order:
// a turn that left nothing behind is re-armed sooner than one that named someone,
// and a review still gets its own window. A change here is a deliberate policy
// change, not an accident.
func TestConcludeChildDoesNotAcceptHistoricalGate(t *testing.T) {
	fx := newChildDoneFixture(t, "in_progress")
	ctx := context.Background()
	id, err := util.ParseUUID(fx.child.ID)
	if err != nil {
		t.Fatal(err)
	}
	child, err := testHandler.Queries.GetIssue(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.postSystemComment(ctx, child, "✅ 验收门：历史记录，后来发现缺口"); err != nil {
		t.Fatal(err)
	}
	if !testHandler.concludeChild(ctx, child, livenessSnapshot{ID: fx.child.ID, AgentBody: "⏳ 等待确认：验收证据不足"}) {
		t.Fatal("exhausted child was not concluded")
	}
	updated, err := testHandler.Queries.GetIssue(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "cancelled" {
		t.Fatalf("historical gate closed stalled child as %s, want cancelled", updated.Status)
	}
}

func TestConcludeChildRetriesAfterCommentSucceededButStatusFailed(t *testing.T) {
	fx := newChildDoneFixture(t, "in_progress")
	ctx := context.Background()
	id, err := util.ParseUUID(fx.child.ID)
	if err != nil {
		t.Fatal(err)
	}
	child, err := testHandler.Queries.GetIssue(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.postSystemComment(ctx, child, "previous interrupted attempt <!-- concluded:"+fx.child.ID+" -->"); err != nil {
		t.Fatal(err)
	}
	if !testHandler.concludeChild(ctx, child, livenessSnapshot{ID: fx.child.ID, AgentBody: "⏳ 等待确认：仍未交付"}) {
		t.Fatal("comment from a failed prior attempt prevented status retry")
	}
	updated, err := testHandler.Queries.GetIssue(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "cancelled" {
		t.Fatalf("retried child status = %s, want cancelled", updated.Status)
	}
	var markers int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE '%<!-- concluded:%'`, id).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if markers != 1 {
		t.Fatalf("retry wrote %d conclusion comments, want one", markers)
	}
}

func TestClimbTopRungRepairsMissingOwnerNotificationOnce(t *testing.T) {
	ctx := context.Background()
	id := dbfx.Issue(t, "legacy top rung", map[string]any{"status": "in_progress"})
	s := livenessSnapshot{ID: id, Rung: 3, AgentAt: time.Now().Add(-time.Hour), AgentBody: "⏳ 等待确认：需裁决"}
	if !testHandler.climb(ctx, s) {
		t.Fatal("legacy top rung did not notify the owner")
	}
	var notifications int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE '%<!-- owner-asked:ladder:%'`, id).Scan(&notifications); err != nil {
		t.Fatal(err)
	}
	if notifications != 1 {
		t.Fatalf("owner notifications = %d, want 1", notifications)
	}
	if testHandler.climb(ctx, s) {
		t.Fatal("top rung notified owner twice")
	}
}

func TestClimbChildMakesEscalationVisibleOnParent(t *testing.T) {
	ctx := context.Background()
	leaderID := dbfx.Agent(t, "parent escalation leader", testRuntimeID)
	workerID := dbfx.Agent(t, "child escalation worker", testRuntimeID)
	parentID := dbfx.Issue(t, "parent needs global view", map[string]any{
		"status": "in_progress", "assignee_type": "agent", "assignee_id": leaderID,
	})
	childID := dbfx.Issue(t, "child needs decision", map[string]any{
		"status": "in_progress", "assignee_type": "agent", "assignee_id": workerID, "parent_issue_id": parentID,
	})
	s := livenessSnapshot{ID: childID, Status: "in_progress", Rung: 1,
		AgentAt: time.Now().Add(-time.Hour), AgentBody: "⏳ 等待确认：测试环境如何处理"}
	if !testHandler.climb(ctx, s) {
		t.Fatal("child escalation did not enqueue parent leader")
	}
	var parentNotes int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE '%<!-- child-escalation:%'`, parentID).Scan(&parentNotes); err != nil {
		t.Fatal(err)
	}
	if parentNotes != 1 {
		t.Fatalf("parent escalation notes=%d, want one", parentNotes)
	}
}

func TestClimbChildWithoutExecutableParentLeaderEscalatesToOwner(t *testing.T) {
	ctx := context.Background()
	workerID := dbfx.Agent(t, "child without parent leader worker", testRuntimeID)
	parentID := dbfx.Issue(t, "parent without executable leader", map[string]any{"status": "in_progress"})
	childID := dbfx.Issue(t, "child awaiting parent decision", map[string]any{
		"status": "in_progress", "assignee_type": "agent", "assignee_id": workerID, "parent_issue_id": parentID,
	})
	s := livenessSnapshot{ID: childID, Status: "in_progress", Rung: 1,
		AgentAt: time.Now().Add(-time.Hour), AgentBody: "⏳ 等待确认：缺父级负责人"}
	if !testHandler.climb(ctx, s) {
		t.Fatal("missing parent leader left child at the same rung")
	}
	var ownerNotes int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE '%<!-- owner-asked:ladder:%'`, childID).Scan(&ownerNotes); err != nil {
		t.Fatal(err)
	}
	if ownerNotes != 1 {
		t.Fatalf("owner escalation notes=%d, want one", ownerNotes)
	}
}

func TestLivenessCandidateIncludesParentWithUnstartedChildren(t *testing.T) {
	ctx := context.Background()
	agentID := dbfx.Agent(t, "liveness parent leader", testRuntimeID)
	parentID := dbfx.Issue(t, "liveness parent", map[string]any{
		"status": "in_progress", "assignee_type": "agent", "assignee_id": agentID,
	})
	dbfx.Issue(t, "unstarted child one", map[string]any{"status": "todo", "parent_issue_id": parentID})
	dbfx.Issue(t, "unstarted child two", map[string]any{"status": "backlog", "parent_issue_id": parentID})
	dbfx.Comment(t, parentID, "waiting on children", map[string]any{
		"author_type": "agent", "author_id": agentID,
	})
	rows, err := testPool.Query(ctx, livenessIssuesSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s livenessSnapshot
		if err := rows.Scan(&s.ID, &s.Status, &s.OpenKids, &s.UnstartedKids, &s.OrphanKids,
			&s.AgentBody, &s.AgentAt, &s.MarkerAt, &s.MemberAt, &s.Rung, &s.GateAt); err != nil {
			t.Fatal(err)
		}
		if s.ID == parentID {
			if s.OpenKids != 2 || s.UnstartedKids != 2 {
				t.Fatalf("parent children = %d open, %d unstarted; want 2/2", s.OpenKids, s.UnstartedKids)
			}
			if action := classifyLiveness(s, time.Now().Add(time.Hour), DefaultLivenessPolicy()); action != actWakeParent {
				t.Fatalf("parent with no human reply classified as %v, want parent wake", action)
			}
			return
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatal("parent with two unstarted children was absent from liveness candidates")
}

func TestLivenessCandidateIncludesParentWithUnassignedChild(t *testing.T) {
	ctx := context.Background()
	leaderID := dbfx.Agent(t, "orphan child parent leader", testRuntimeID)
	parentID := dbfx.Issue(t, "parent with orphan child", map[string]any{
		"status": "in_progress", "assignee_type": "agent", "assignee_id": leaderID,
	})
	dbfx.Issue(t, "unassigned started child", map[string]any{
		"status": "in_progress", "parent_issue_id": parentID,
	})
	dbfx.Comment(t, parentID, "等子任务", map[string]any{"author_type": "agent", "author_id": leaderID})
	rows, err := testPool.Query(ctx, livenessIssuesSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s livenessSnapshot
		if err := rows.Scan(&s.ID, &s.Status, &s.OpenKids, &s.UnstartedKids, &s.OrphanKids,
			&s.AgentBody, &s.AgentAt, &s.MarkerAt, &s.MemberAt, &s.Rung, &s.GateAt); err != nil {
			t.Fatal(err)
		}
		if s.ID == parentID {
			if s.OrphanKids != 1 || s.OpenKids != 1 {
				t.Fatalf("parent children = %d open, %d orphan; want 1/1", s.OpenKids, s.OrphanKids)
			}
			parentUUID, err := util.ParseUUID(parentID)
			if err != nil {
				t.Fatal(err)
			}
			parent, err := testHandler.Queries.GetIssue(ctx, parentUUID)
			if err != nil {
				t.Fatal(err)
			}
			if label := testHandler.openPendingChildLabel(ctx, parent); !strings.Contains(label, "unassigned started child") {
				t.Fatalf("orphan child absent from parent action summary: %q", label)
			}
			return
		}
	}
	t.Fatal("parent with unassigned child was absent from liveness candidates")
}

func TestParentWakeIncludesChildWaitingForRetry(t *testing.T) {
	ctx := context.Background()
	agentID := dbfx.Agent(t, "child retry agent", testRuntimeID)
	parentID := dbfx.Issue(t, "parent waiting for retry", map[string]any{"status": "in_progress"})
	childID := dbfx.Issue(t, "retryable child", map[string]any{"status": "todo", "parent_issue_id": parentID})
	dbfx.Task(t, agentID, testutil.Cols{"issue_id": childID, "status": "completed", "runtime_id": testRuntimeID})
	parentUUID, err := util.ParseUUID(parentID)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := testHandler.Queries.GetIssue(ctx, parentUUID)
	if err != nil {
		t.Fatal(err)
	}
	if label := testHandler.openPendingChildLabel(ctx, parent); !strings.Contains(label, "retryable child") {
		t.Fatalf("parent wake omitted child awaiting retry: %q", label)
	}
}

func TestHumanReplyResetsExhaustedLivenessLadder(t *testing.T) {
	ctx := context.Background()
	agentID := dbfx.Agent(t, "ladder reset agent", testRuntimeID)
	issueID := dbfx.Issue(t, "owner replied after escalation", map[string]any{
		"status": "in_progress", "assignee_type": "agent", "assignee_id": agentID,
	})
	dbfx.Comment(t, issueID, "⏳ 等待确认：需要裁决", map[string]any{"author_type": "agent", "author_id": agentID})
	dbfx.Comment(t, issueID, "<!-- rung:3:"+issueID+" --><!-- owner-asked:ladder:"+issueID+" -->", map[string]any{"author_type": "system"})
	dbfx.Comment(t, issueID, "按现有测试环境继续")
	rows, err := testPool.Query(ctx, livenessIssuesSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s livenessSnapshot
		if err := rows.Scan(&s.ID, &s.Status, &s.OpenKids, &s.UnstartedKids, &s.OrphanKids,
			&s.AgentBody, &s.AgentAt, &s.MarkerAt, &s.MemberAt, &s.Rung, &s.GateAt); err != nil {
			t.Fatal(err)
		}
		if s.ID == issueID {
			if s.Rung != 0 || s.MemberAt.IsZero() {
				t.Fatalf("reply should reset ladder, got rung=%d member=%v", s.Rung, s.MemberAt)
			}
			return
		}
	}
	t.Fatal("issue with owner reply absent from liveness candidates")
}

func TestChildProgressResetsExhaustedParentLivenessLadder(t *testing.T) {
	ctx := context.Background()
	agentID := dbfx.Agent(t, "parent after child progress agent", testRuntimeID)
	parentID := dbfx.Issue(t, "parent receives new child result", map[string]any{
		"status": "in_review", "assignee_type": "agent", "assignee_id": agentID,
	})
	dbfx.Comment(t, parentID, "✅ 验收门：旧父级结论", map[string]any{
		"author_type": "agent", "author_id": agentID, "created_at": time.Now().Add(-time.Hour),
	})
	dbfx.Comment(t, parentID, "已通知所有者 <!-- rung:3:"+parentID+" --><!-- owner-asked:ladder:"+parentID+" -->", map[string]any{
		"author_type": "system", "created_at": time.Now().Add(-30 * time.Minute),
	})
	dbfx.Comment(t, parentID, "子任务结果回传 <!-- handoff:child-result -->", map[string]any{
		"author_type": "system", "created_at": time.Now().Add(-20 * time.Minute),
	})
	rows, err := testPool.Query(ctx, livenessIssuesSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s livenessSnapshot
		if err := rows.Scan(&s.ID, &s.Status, &s.OpenKids, &s.UnstartedKids, &s.OrphanKids,
			&s.AgentBody, &s.AgentAt, &s.MarkerAt, &s.MemberAt, &s.Rung, &s.GateAt); err != nil {
			t.Fatal(err)
		}
		if s.ID == parentID {
			if s.Rung != 0 || hasLivenessEvent(s.MarkerAt) {
				t.Fatalf("new child result did not reset parent ladder: rung=%d marker=%v", s.Rung, s.MarkerAt)
			}
			if action := classifyLiveness(s, time.Now(), DefaultLivenessPolicy()); action != actCloseReview {
				t.Fatalf("parent after child result classified as %v, want review repair", action)
			}
			return
		}
	}
	t.Fatal("parent with new child result absent from liveness candidates")
}

func TestDeferredReconnectTaskKeepsIssueOutOfLivenessAndBarrierSweeps(t *testing.T) {
	ctx := context.Background()
	agentID := dbfx.Agent(t, "long offline continuation agent", testRuntimeID)
	issueID := dbfx.Issue(t, "waiting for runtime reconnect", map[string]any{
		"status": "in_progress", "assignee_type": "agent", "assignee_id": agentID,
	})
	dbfx.Comment(t, issueID, "✅ 验收门：旧结论", map[string]any{
		"author_type": "agent", "author_id": agentID, "created_at": time.Now().Add(-time.Hour),
	})
	dbfx.Task(t, agentID, testutil.Cols{
		"issue_id": issueID, "status": "deferred", "runtime_id": testRuntimeID,
		"fire_at": time.Now().Add(-time.Hour),
	})
	rows, err := testPool.Query(ctx, livenessIssuesSQL)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s livenessSnapshot
		if err := rows.Scan(&s.ID, &s.Status, &s.OpenKids, &s.UnstartedKids, &s.OrphanKids,
			&s.AgentBody, &s.AgentAt, &s.MarkerAt, &s.MemberAt, &s.Rung, &s.GateAt); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if s.ID == issueID {
			rows.Close()
			t.Fatal("waiting retry was treated as an idle issue")
		}
	}
	rows.Close()
	if moved := testHandler.SweepBarrierLiveness(ctx); moved != 0 {
		t.Fatalf("waiting retry's old gate was accepted, moved=%d", moved)
	}
}

func TestLivenessCandidateIncludesAssignedIssueWithoutAgentComment(t *testing.T) {
	ctx := context.Background()
	agentID := dbfx.Agent(t, "unstarted assignee", testRuntimeID)
	issueID := dbfx.Issue(t, "assigned but never dispatched", map[string]any{
		"status": "in_progress", "assignee_type": "agent", "assignee_id": agentID,
	})
	rows, err := testPool.Query(ctx, livenessIssuesSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s livenessSnapshot
		if err := rows.Scan(&s.ID, &s.Status, &s.OpenKids, &s.UnstartedKids, &s.OrphanKids,
			&s.AgentBody, &s.AgentAt, &s.MarkerAt, &s.MemberAt, &s.Rung, &s.GateAt); err != nil {
			t.Fatal(err)
		}
		if s.ID == issueID {
			if s.AgentBody != "" || s.AgentAt.IsZero() {
				t.Fatalf("unexpected starting snapshot: %+v", s)
			}
			return
		}
	}
	t.Fatal("assigned issue without agent comment was invisible to liveness")
}

func TestLivenessCandidateResolvesCustomStatusCategories(t *testing.T) {
	ctx := context.Background()
	dbfx.Insert(t, "issue_status", testutil.Cols{
		"workspace_id": testWorkspaceID, "key": "liveness_started", "name": "Liveness Started", "category": "started", "color": "#123456",
	})
	dbfx.Insert(t, "issue_status", testutil.Cols{
		"workspace_id": testWorkspaceID, "key": "liveness_done", "name": "Liveness Done", "category": "done", "color": "#123456",
	})
	agentID := dbfx.Agent(t, "custom status agent", testRuntimeID)
	parentID := dbfx.Issue(t, "custom status parent", map[string]any{
		"status": "liveness_started", "assignee_type": "agent", "assignee_id": agentID,
	})
	dbfx.Issue(t, "custom done child", map[string]any{"status": "liveness_done", "parent_issue_id": parentID})
	rows, err := testPool.Query(ctx, livenessIssuesSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s livenessSnapshot
		if err := rows.Scan(&s.ID, &s.Status, &s.OpenKids, &s.UnstartedKids, &s.OrphanKids,
			&s.AgentBody, &s.AgentAt, &s.MarkerAt, &s.MemberAt, &s.Rung, &s.GateAt); err != nil {
			t.Fatal(err)
		}
		if s.ID == parentID {
			if s.OpenKids != 0 {
				t.Fatalf("custom done child counted as open: %d", s.OpenKids)
			}
			return
		}
	}
	t.Fatal("custom started parent was absent from liveness candidates")
}

func TestLivenessActionAcceptsCustomStartedStatus(t *testing.T) {
	ctx := context.Background()
	dbfx.Insert(t, "issue_status", testutil.Cols{
		"workspace_id": testWorkspaceID, "key": "custom_live_action", "name": "Custom Live Action", "category": "started", "color": "#123456",
	})
	agentID := dbfx.Agent(t, "custom action agent", testRuntimeID)
	issueID := dbfx.Issue(t, "custom active issue", map[string]any{
		"status": "custom_live_action", "assignee_type": "agent", "assignee_id": agentID,
	})
	if _, _, ok := testHandler.loadForAction(ctx, livenessSnapshot{ID: issueID}); !ok {
		t.Fatal("custom started issue was selected but rejected before repair")
	}
}

func TestEnvironmentRulingIsIssuedOnceForActualParks(t *testing.T) {
	ctx := context.Background()
	agentID := dbfx.Agent(t, "environment ruling agent", testRuntimeID)
	issueID := dbfx.Issue(t, "environment ruling issue", map[string]any{"status": "in_progress"})
	park := func() {
		dbfx.Comment(t, issueID, "⏳ 等待确认：MySQL 测试环境在哪里？", map[string]any{
			"author_type": "agent", "author_id": agentID,
		})
	}
	candidate := func() bool {
		rows, err := testPool.Query(ctx, environmentParksSQL, 0, 200)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, body string
			var at time.Time
			var count int
			if err := rows.Scan(&id, &body, &at, &count); err != nil {
				t.Fatal(err)
			}
			if id == issueID {
				return true
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return false
	}
	// Mere discussion of the environment must not consume the park budget.
	dbfx.Comment(t, issueID, "MySQL 环境已发现，继续执行", map[string]any{
		"author_type": "agent", "author_id": agentID,
	})
	if candidate() {
		t.Fatal("ordinary environment discussion triggered default ruling")
	}
	park()
	park()
	if !candidate() {
		t.Fatal("second actual environment park did not trigger one ruling")
	}
	dbfx.Comment(t, issueID, "裁决 <!-- default-ruling:env:"+issueID+" -->", map[string]any{
		"author_type": "system", "author_id": testUserID,
	})
	park()
	if candidate() {
		t.Fatal("a later park triggered a duplicate default ruling")
	}
}

func TestLivenessPolicyOrdering(t *testing.T) {
	pol := DefaultLivenessPolicy()
	if pol.DeadEndIdle >= pol.AddressedIdle {
		t.Errorf("dead end %v must be shorter than addressed %v", pol.DeadEndIdle, pol.AddressedIdle)
	}
	if pol.ReviewIdle <= pol.DeadEndIdle {
		t.Errorf("review %v should give the assignee at least the dead-end window %v", pol.ReviewIdle, pol.DeadEndIdle)
	}
	if pol.HumanRequestGrace > pol.WaitIdle {
		t.Errorf("a human request should reach the owner (%v) before the agent ladder climbs (%v)",
			pol.HumanRequestGrace, pol.WaitIdle)
	}
	if pol.Batch <= 0 {
		t.Errorf("batch must be positive, got %d", pol.Batch)
	}
}
