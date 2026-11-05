package handler

import "testing"

// A parked issue is one that legitimately waits on a human. Everything else
// that merely opens with ⏳/⛔ is work that stopped, and must stay visible to the
// liveness sweeps — otherwise an agent's own "this round failed" note freezes
// the issue while every check reports "nothing to do".
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

// The gate sweep must not close an in_progress issue whose own last word is a
// marker. Narrowing this to the sanctioned wait forms once turned "⛔ 本轮未能…"
// into an automatic delivery, so the rule is pinned here.
func TestGateAutoAdvanceBlocked(t *testing.T) {
	cases := []struct {
		name        string
		status      string
		lastComment string
		want        bool
	}{
		{"wait marker while in progress", "in_progress", "⏳ 等待确认：是否支持 v1？", true},
		{"approval marker while in progress", "in_progress", "⛔ 需要人工审批：删除线上表", true},
		{"failed round while in progress", "in_progress", "⛔ 本轮未能在当前 checkout 实施 T16", true},
		{"gate rejection while in progress", "in_progress", "⛔ 验收门未过：缺少记录", true},
		{"plain progress while in progress", "in_progress", "已完成 T16 实现并补证", false},
		{"gate marker while in review", "in_review", "✅ 验收门：AC 3/3", false},
		{"rejection after gate in review", "in_review", "⛔ 验收门未过：缺少记录", false},
	}
	for _, tc := range cases {
		if got := gateAutoAdvanceBlocked(tc.status, tc.lastComment); got != tc.want {
			t.Errorf("%s: gateAutoAdvanceBlocked(%q, %q) = %v, want %v", tc.name, tc.status, tc.lastComment, got, tc.want)
		}
	}
}
