package handler

import (
	"strings"
	"testing"
	"time"
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
	now := time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	gate := ago(30 * time.Minute)

	cases := []struct {
		name string
		snap livenessSnapshot
		want livenessAction
	}{
		{
			name: "current gate is accepted, not our job",
			snap: livenessSnapshot{Status: "in_review", LastAt: ago(time.Hour), LastAuthor: "agent",
				LastBody: "✅ 验收门：AC 3/3", GateAt: gate, LastAgentAt: gate},
			want: actAccept,
		},
		{
			name: "review with a superseded gate",
			snap: livenessSnapshot{Status: "in_review", LastAt: ago(time.Hour), LastAuthor: "agent",
				LastBody: "本轮未能完成", GateAt: gate, LastAgentAt: ago(5 * time.Minute)},
			want: actCloseReview,
		},
		{
			name: "review too fresh to nudge",
			snap: livenessSnapshot{Status: "in_review", LastAt: ago(time.Minute), LastAuthor: "agent",
				LastBody: "已置 in_review", LastAgentAt: ago(time.Minute)},
			want: actNone,
		},
		{
			name: "parent waiting on a never-started child",
			snap: livenessSnapshot{Status: "in_progress", OpenKids: 1, UnstartedKids: 1,
				LastAt: ago(time.Hour), LastAuthor: "agent", LastBody: "等子任务"},
			want: actWakeParent,
		},
		{
			name: "parent waiting on a running child is fine",
			snap: livenessSnapshot{Status: "in_progress", OpenKids: 1, UnstartedKids: 0,
				LastAt: ago(time.Hour), LastAuthor: "agent", LastBody: "等子任务"},
			want: actNone,
		},
		{
			name: "park that named nobody climbs at once",
			snap: livenessSnapshot{Status: "in_progress", LastAt: ago(time.Minute), LastAuthor: "agent",
				LastBody: "⏳ 等待确认：请把 write_files 扩到 .gitignore"},
			want: actClimb,
		},
		{
			name: "park addressed to someone is a real wait",
			snap: livenessSnapshot{Status: "in_progress", LastAt: ago(5 * time.Minute), LastAuthor: "agent",
				LastBody: "⏳ 等待确认：请 [@负责人](mention://agent/x) 裁决写集"},
			want: actNone,
		},
		{
			name: "park addressed to a human is a real wait, however stale",
			snap: livenessSnapshot{Status: "in_progress", LastAt: ago(3 * time.Hour), LastAuthor: "agent",
				LastBody: "⏳ 等待确认：[@owner](mention://member/abc) 请提供审核分支"},
			want: actNone,
		},
		{
			name: "stale addressed park climbs",
			snap: livenessSnapshot{Status: "in_progress", LastAt: ago(time.Hour), LastAuthor: "agent",
				LastBody: "⏳ 等待确认：请 [@负责人](mention://agent/x) 裁决写集"},
			want: actClimb,
		},
		{
			name: "our own nudge as the last word climbs",
			snap: livenessSnapshot{Status: "in_progress", LastAt: ago(20 * time.Minute), LastAuthor: "system",
				LastBody: "[@研发交付小队](mention://squad/x) 停滞自检：本 issue 已 15 分钟没有运行任务"},
			want: actClimb,
		},
		{
			name: "nudge too fresh to climb",
			snap: livenessSnapshot{Status: "in_progress", LastAt: ago(2 * time.Minute), LastAuthor: "system",
				LastBody: "[@研发交付小队](mention://squad/x) 停滞自检"},
			want: actNone,
		},
		{
			name: "dead end climbs to the assignee within minutes",
			snap: livenessSnapshot{Status: "in_progress", LastAt: ago(4 * time.Minute), LastAuthor: "agent",
				LastBody: "T01 继续 BLOCKED，W1/T02 仍锁定"},
			want: actWakeAssignee,
		},
		{
			name: "dead end too fresh",
			snap: livenessSnapshot{Status: "in_progress", LastAt: ago(time.Minute), LastAuthor: "agent",
				LastBody: "T01 继续 BLOCKED"},
			want: actNone,
		},
		{
			name: "turn that named an agent gets the longer window",
			snap: livenessSnapshot{Status: "in_progress", LastAt: ago(10 * time.Minute), LastAuthor: "agent",
				LastBody: "请 [@评审](mention://agent/x) 投票"},
			want: actNone,
		},
		{
			name: "named agent never took it",
			snap: livenessSnapshot{Status: "in_progress", LastAt: ago(time.Hour), LastAuthor: "agent",
				LastBody: "请 [@评审](mention://agent/x) 投票"},
			want: actWakeAssignee,
		},
	}

	for _, tc := range cases {
		if got := classifyLiveness(tc.snap, now); got != tc.want {
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
		{"in_review", "⛔ 验收门未过：缺少记录", false},
	}
	for _, tc := range cases {
		if got := gateAutoAdvanceBlocked(tc.status, tc.lastComment); got != tc.want {
			t.Errorf("gateAutoAdvanceBlocked(%q, %q) = %v, want %v", tc.status, tc.lastComment, got, tc.want)
		}
	}
}

func TestGateSupersededByAgent(t *testing.T) {
	gate := time.Date(2026, 11, 5, 17, 17, 0, 0, time.UTC)
	if gateSupersededByAgent(gate, gate) {
		t.Error("gate as the last agent word must not be superseded")
	}
	if !gateSupersededByAgent(gate, gate.Add(time.Hour)) {
		t.Error("an agent speaking after the gate supersedes it")
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
		{actWakeParent, 3, actWakeParent},     // parent actions are not re-asked
		{actNone, 3, actNone},
		{actAccept, 1, actAccept},
	}
	for _, tc := range cases {
		if got := applyLadder(tc.action, tc.rung); got != tc.want {
			t.Errorf("applyLadder(%v, rung %d) = %v, want %v", tc.action, tc.rung, got, tc.want)
		}
	}
}
