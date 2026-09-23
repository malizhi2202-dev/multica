package tuitui

// outbound_chat_db_test.go exercises the chat-done subscriber: the durable
// channel_task_delivery row frozen at enqueue time is the whole routing
// input, so the subscriber test seeds one directly and asserts the reply
// actually leaves through the installation's stored host + port, in the
// payload shape the stored (chat_type, channel_chat_id) pair reverse-parses
// to — including the teams case that no in-memory map may decide.

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// seedChannelTurnFixture is one completed agent turn addressed at a
// conversation: binding for the chat, task, and the delivery row the task
// service would have frozen. The delivery row — not the binding — is what
// the subscriber must route from, so the test varies it.
func (f *tuituiRouteFixture) seedChannelTurn(t *testing.T, channelChatID, chatType, threadID string) (taskID, sessionID string) {
	t.Helper()
	sessionID = f.fx.Insert(t, "chat_session", testutil.Cols{
		"workspace_id": f.workspaceIDStr,
		"agent_id":     f.agentIDStr,
		"creator_id":   f.userIDStr,
		"title":        "tuitui turn",
		"status":       "active",
	})
	bindingID := f.fx.Insert(t, "channel_chat_session_binding", testutil.Cols{
		"chat_session_id": sessionID,
		"installation_id": f.installationStr,
		"channel_type":    string(TypeTuitui),
		"channel_chat_id": channelChatID,
		"chat_type":       chatType,
	})
	// agent_task_queue's "active requires runtime" CHECK wants runtime_id
	// or completed_at; a delivered channel reply is a finished,
	// runtime-backed run.
	taskID = f.fx.Task(t, f.agentIDStr, testutil.Cols{
		"status":          "completed",
		"chat_session_id": sessionID,
		"runtime_id":      f.runtimeIDStr,
		"completed_at":    testutil.Raw("now()"),
	})
	f.fx.InsertNoID(t, "channel_task_delivery", testutil.Cols{
		"task_id":           taskID,
		"binding_id":        bindingID,
		"installation_id":   f.installationStr,
		"channel_type":      string(TypeTuitui),
		"channel_chat_id":   channelChatID,
		"chat_type":         chatType,
		"channel_thread_id": threadID,
		"route_revision":    1,
		"config":            testutil.Raw(`'{}'::jsonb`),
	}, `task_id = $1`, taskID)
	return taskID, sessionID
}

func tuituiDoneEvent(taskID, sessionID, content string) events.Event {
	return events.Event{
		Type: protocol.EventChatDone,
		Payload: protocol.ChatDonePayload{
			ChatSessionID: sessionID, TaskID: taskID, Content: content,
		},
	}
}

func TestTuituiOutboundDeliversTeamsReplyAsMarkdown(t *testing.T) {
	f := newTuituiRouteFixture(t)
	taskID, sessionID := f.seedChannelTurn(t, "teams_eng_release_post-9", "group", "post-9")

	o := NewOutbound(f.q, nil, testDiscardLogger())
	o.httpClient = f.stubTLS
	if err := o.processEvent(context.Background(), tuituiDoneEvent(taskID, sessionID, "**hi** from the agent")); err != nil {
		t.Fatalf("processEvent: %v", err)
	}

	reqs := f.stub.all()
	if len(reqs) != 1 {
		t.Fatalf("sends = %d, want 1: %+v", len(reqs), reqs)
	}
	req := reqs[0]
	if req.Path != "/robot"+pathSend {
		t.Errorf("path = %q, want /robot%s — the URL must come from the stored host:port", req.Path, pathSend)
	}
	if req.AppID != f.appID || req.Secret != "sec-1" {
		t.Errorf("query auth = %q/%q, want the stored credentials", req.AppID, req.Secret)
	}
	if req.Body.MsgType != "richtext/markdown" || req.Body.RichText == nil ||
		req.Body.RichText.Markdown != "**hi** from the agent" {
		t.Fatalf("teams replies must be markdown richtext, got %+v", req.Body)
	}
	if len(req.Body.ToTeams) != 1 || req.Body.ToTeams[0].TeamID != "eng" ||
		req.Body.ToTeams[0].ChannelID != "release" || req.Body.ToTeams[0].ParentID != "post-9" {
		t.Errorf("toteams = %+v, want the triple reverse-parsed from teams_eng_release_post-9", req.Body.ToTeams)
	}
	if len(req.Body.ToGroups) != 0 {
		t.Errorf("a teams reply must not also target togroups: %v", req.Body.ToGroups)
	}
}

func TestTuituiOutboundDeliversP2PReplyAsText(t *testing.T) {
	f := newTuituiRouteFixture(t)
	taskID, sessionID := f.seedChannelTurn(t, "alice-dm", "p2p", "")

	o := NewOutbound(f.q, nil, testDiscardLogger())
	o.httpClient = f.stubTLS
	if err := o.processEvent(context.Background(), tuituiDoneEvent(taskID, sessionID, "plain answer")); err != nil {
		t.Fatalf("processEvent: %v", err)
	}

	reqs := f.stub.all()
	if len(reqs) != 1 {
		t.Fatalf("sends = %d, want 1", len(reqs))
	}
	req := reqs[0]
	if req.Body.MsgType != "text" || req.Body.Text == nil || req.Body.Text.Content != "plain answer" {
		t.Fatalf("p2p replies must be plain text, got %+v", req.Body)
	}
	if len(req.Body.ToUsers) != 1 || string(req.Body.ToUsers[0]) != `"alice-dm"` {
		t.Errorf("tousers = %v, want the p2p chat id", req.Body.ToUsers)
	}
}

func TestTuituiOutboundIgnoresForeignChannels(t *testing.T) {
	f := newTuituiRouteFixture(t)
	taskID, sessionID := f.seedChannelTurn(t, "alice-dm", "p2p", "")
	// Re-point the delivery at another platform: the subscriber must stay
	// silent so each channel's subscriber answers its own rows only.
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE channel_task_delivery SET channel_type = 'dingtalk' WHERE task_id = $1`, taskID); err != nil {
		t.Fatalf("mutate delivery: %v", err)
	}

	o := NewOutbound(f.q, nil, testDiscardLogger())
	o.httpClient = f.stubTLS
	if err := o.processEvent(context.Background(), tuituiDoneEvent(taskID, sessionID, "not ours")); err != nil {
		t.Fatalf("processEvent: %v", err)
	}
	if reqs := f.stub.all(); len(reqs) != 0 {
		t.Errorf("sends = %d, want 0 for a foreign channel delivery", len(reqs))
	}
}

func TestTuituiOutboundSilentWhileRetryPending(t *testing.T) {
	f := newTuituiRouteFixture(t)
	taskID, sessionID := f.seedChannelTurn(t, "alice-dm", "p2p", "")

	o := NewOutbound(f.q, nil, testDiscardLogger())
	o.httpClient = f.stubTLS
	// task:failed while an automatic retry is pending carries no deliverable
	// text; the retry reports its own outcome.
	err := o.processEvent(context.Background(), events.Event{
		Type:          protocol.EventTaskFailed,
		TaskID:        taskID,
		ChatSessionID: sessionID,
		Payload: map[string]any{
			"task_id":         taskID,
			"chat_session_id": sessionID,
			"retry_pending":   true,
			"error":           "model unavailable",
		},
	})
	if err != nil {
		t.Fatalf("processEvent: %v", err)
	}
	if reqs := f.stub.all(); len(reqs) != 0 {
		t.Errorf("sends = %d, want 0 while a retry is pending", len(reqs))
	}
}

func TestTuituiOutboundDeliversFailureNotice(t *testing.T) {
	f := newTuituiRouteFixture(t)
	taskID, sessionID := f.seedChannelTurn(t, "alice-dm", "p2p", "")

	o := NewOutbound(f.q, nil, testDiscardLogger())
	o.httpClient = f.stubTLS
	if err := o.processEvent(context.Background(), events.Event{
		Type:          protocol.EventTaskFailed,
		TaskID:        taskID,
		ChatSessionID: sessionID,
		Payload: map[string]any{
			"task_id":         taskID,
			"chat_session_id": sessionID,
			"error":           "runtime crashed",
		},
	}); err != nil {
		t.Fatalf("processEvent: %v", err)
	}
	reqs := f.stub.all()
	if len(reqs) != 1 {
		t.Fatalf("sends = %d, want the failure notice", len(reqs))
	}
	if reqs[0].Body.Text == nil || reqs[0].Body.Text.Content != "⚠️ runtime crashed" {
		t.Errorf("failure notice = %+v", reqs[0].Body)
	}
}

// A revoked installation must not receive the reply either — the row's
// status is checked at delivery time, exactly like the other channels.
func TestTuituiOutboundSkipsRevokedInstallation(t *testing.T) {
	f := newTuituiRouteFixture(t)
	taskID, sessionID := f.seedChannelTurn(t, "alice-dm", "p2p", "")
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE channel_installation SET status = 'revoked' WHERE id = $1`, f.installationStr); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	o := NewOutbound(f.q, nil, testDiscardLogger())
	o.httpClient = f.stubTLS
	if err := o.processEvent(context.Background(), tuituiDoneEvent(taskID, sessionID, "too late")); err != nil {
		t.Fatalf("processEvent: %v", err)
	}
	if reqs := f.stub.all(); len(reqs) != 0 {
		t.Errorf("sends = %d, want 0 after revocation", len(reqs))
	}
}
