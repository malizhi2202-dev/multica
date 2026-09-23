package tuitui

// resolvers_test.go — the non-DB half of the resolver layer: the reverse
// parse matrix for composite teams ids, the durable (chat_type, chat id)
// → payload-kind derivation the outbound paths share, the ResolverSet
// shape the engine.Router requires (nil Media / Typing included), and the
// replier's per-conversation send shapes driven through a fake platform row.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestTuituiKindForSourceMatrix(t *testing.T) {
	cases := []struct {
		chatType, chatID string
		want             chatKind
	}{
		{string(channel.ChatTypeP2P), "alice", kindP2P},
		{string(channel.ChatTypeGroup), "778899", kindGroup},
		// Teams rides ChatTypeGroup (the engine enum is closed) and is
		// recognized ONLY by the prefix — the cross-restart rule.
		{string(channel.ChatTypeGroup), "teams_t_c", kindTeams},
		{string(channel.ChatTypeGroup), "teams_t_c_th", kindTeams},
		// Even a mislabelled p2p row cannot hide a teams conversation: the
		// prefix outranks the chat type.
		{string(channel.ChatTypeP2P), "teams_t_c_th", kindTeams},
		// A group id that merely contains the word is not a teams composite.
		{string(channel.ChatTypeGroup), "teams", kindGroup},
		{string(channel.ChatTypeGroup), "1234teams_5", kindGroup},
	}
	for _, c := range cases {
		if got := kindForSource(c.chatType, c.chatID); got != c.want {
			t.Errorf("kindForSource(%q, %q) = %d, want %d", c.chatType, c.chatID, got, c.want)
		}
	}
}

func TestTuituiSplitTeamsChatIDMatrix(t *testing.T) {
	cases := []struct {
		chatID                  string
		team, channelID, thread string
		ok                      bool
	}{
		{"teams_t1_c2_th3", "t1", "c2", "th3", true},
		{"teams_t1_c2", "t1", "c2", "", true},
		{"teams_onlyteam", "", "", "", false},
		{"teams_", "", "", "", false},
		{"teams__c2", "", "", "", false},
		{"teams_t1_", "", "", "", false},
		// Embedded underscores make the positional parse ambiguous (platform
		// ids carry none): a hard failure, never a guess.
		{"teams_a_b_c_d", "", "", "", false},
		{"1234567890123456", "", "", "", false},
	}
	for _, c := range cases {
		team, channelID, thread, ok := splitTeamsChatID(c.chatID)
		if ok != c.ok || team != c.team || channelID != c.channelID || thread != c.thread {
			t.Errorf("splitTeamsChatID(%q) = (%q %q %q %v), want (%q %q %q %v)",
				c.chatID, team, channelID, thread, ok, c.team, c.channelID, c.thread, c.ok)
		}
	}
}

func TestTuituiResolverSetMeetsRouterRegistrationRequirements(t *testing.T) {
	set := NewTuituiResolverSet(nil, nil, nil)
	if set.Installation == nil || set.Identity == nil || set.Dedup == nil ||
		set.Session == nil || set.Audit == nil {
		t.Fatalf("incomplete set would be silently dropped by Router.Register: %+v", set)
	}
	// Deliberate gaps, pinned so a future edit notices them:
	if set.Media != nil {
		t.Errorf("Media must stay nil until inbound emits MediaRefs")
	}
	if set.Typing != nil {
		t.Errorf("Typing must stay nil (typed-nil guard): %v", set.Typing)
	}
	if set.Replier != nil {
		t.Errorf("Replier = %v, want nil disabled", set.Replier)
	}
	// Stamped on every issue a Tuitui /issue command creates. The literal is
	// deliberate: it is the analytics key and the value migration 510/511
	// added to issue.origin_type's CHECK, so renaming the label here must fail
	// loudly rather than follow the constant.
	if set.OriginType != "tuitui_chat" {
		t.Errorf("OriginType = %q, want tuitui_chat", set.OriginType)
	}
}

func TestTuituiInstallationResolverRejectsForeignRaw(t *testing.T) {
	r := &installationResolver{q: nil}
	// Empty Raw cannot name an installation.
	if _, err := r.ResolveInstallation(context.Background(), channel.InboundMessage{}); err == nil {
		t.Error("empty Raw must error, not route")
	}
	// A stamped envelope without an app_id is the same failure.
	raw, _ := json.Marshal(tuituiRawEvent{Body: eventBody{Event: eventSingleChat}})
	msg := channel.InboundMessage{Raw: raw}
	if _, err := r.ResolveInstallation(context.Background(), msg); err == nil {
		t.Error("missing stamped app_id must error")
	}
}

// fakeMinter records mint attempts so the prompt paths can be asserted
// without a database.
type fakeMinter struct {
	calls int
	by    string
	err   error
}

func (m *fakeMinter) Mint(_ context.Context, _, _ pgtype.UUID, tuituiUserID string) (BindingToken, error) {
	m.calls++
	m.by = tuituiUserID
	if m.err != nil {
		return BindingToken{}, m.err
	}
	return BindingToken{Raw: "tok-123"}, nil
}

func replierTestInstallation(t *testing.T, host string, port int) engine.ResolvedInstallation {
	t.Helper()
	cfgJSON, err := json.Marshal(installConfig{AppID: "app-x", AppSecret: "sec-x", Host: host, Port: port})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return engine.ResolvedInstallation{
		ID:       pgtype.UUID{Bytes: [16]byte{7}, Valid: true},
		Active:   true,
		Platform: db.ChannelInstallation{Config: cfgJSON, Status: "active"},
	}
}

func TestTuituiReplierConversationShapes(t *testing.T) {
	srv, stub, port := newSendStub(t)
	r := NewOutboundReplier(OutboundReplierConfig{
		Binding:    &fakeMinter{},
		HTTPClient: srv.Client(),
		AppURL:     "https://app.test",
		Logger:     testDiscardLogger(),
	})
	inst := replierTestInstallation(t, "127.0.0.1", port)
	ctx := context.Background()

	// A status notice follows the conversation shape of the message that
	// triggered it: p2p → tousers, group → togroups, teams → richtext
	// markdown toteams (reverse-parsed from the stored composite id).
	p2p := channel.InboundMessage{Source: channel.Source{
		ChatID: "alice", ChatType: channel.ChatTypeP2P, SenderID: "alice",
	}}
	group := channel.InboundMessage{Source: channel.Source{
		ChatID: "778899", ChatType: channel.ChatTypeGroup, SenderID: "carol",
	}}
	teams := channel.InboundMessage{Source: channel.Source{
		ChatID: "teams_eng_release_post-9", ChatType: channel.ChatTypeGroup, SenderID: "dave",
		ThreadID: "post-9",
	}}
	for _, msg := range []channel.InboundMessage{p2p, group, teams} {
		r.Reply(ctx, inst, msg, engine.Result{Outcome: engine.OutcomeAgentOffline, Sender: msg.Source.SenderID})
	}

	reqs := stub.all()
	if len(reqs) != 3 {
		t.Fatalf("sends = %d, want one per conversation", len(reqs))
	}
	if len(reqs[0].Body.ToUsers) != 1 || string(reqs[0].Body.ToUsers[0]) != `"alice"` {
		t.Errorf("p2p notice recipients = %v", reqs[0].Body.ToUsers)
	}
	if reqs[0].Body.MsgType != "text" {
		t.Errorf("p2p notice msgtype = %q", reqs[0].Body.MsgType)
	}
	if len(reqs[1].Body.ToGroups) != 1 || string(reqs[1].Body.ToGroups[0]) != `"778899"` {
		t.Errorf("group notice recipients = %v", reqs[1].Body.ToGroups)
	}
	if reqs[2].Body.MsgType != "richtext/markdown" || len(reqs[2].Body.ToTeams) != 1 ||
		reqs[2].Body.ToTeams[0] != (teamTarget{TeamID: "eng", ChannelID: "release", ParentID: "post-9"}) {
		t.Errorf("teams notice payload = %+v", reqs[2].Body)
	}
	for _, req := range reqs {
		if req.Path != "/robot"+pathSend {
			t.Errorf("path = %q, want the stored host:port send URL", req.Path)
		}
	}
}

func TestTuituiReplierBindingPromptIsPrivate(t *testing.T) {
	srv, stub, port := newSendStub(t)
	minter := &fakeMinter{}
	r := NewOutboundReplier(OutboundReplierConfig{
		Binding:    minter,
		HTTPClient: srv.Client(),
		AppURL:     "https://app.test/",
		Logger:     testDiscardLogger(),
	})
	inst := replierTestInstallation(t, "127.0.0.1", port)

	// An unbound sender in a GROUP must still get the single-use link
	// privately — a group broadcast would let anyone redeem it.
	group := channel.InboundMessage{Source: channel.Source{
		ChatID: "778899", ChatType: channel.ChatTypeGroup, SenderID: "carol",
	}}
	r.Reply(context.Background(), inst, group, engine.Result{
		Outcome: engine.OutcomeNeedsBinding, Sender: "carol",
	})

	if minter.calls != 1 || minter.by != "carol" {
		t.Fatalf("mints = %d by %q, want 1 for carol", minter.calls, minter.by)
	}
	reqs := stub.all()
	if len(reqs) != 1 {
		t.Fatalf("sends = %d, want the one prompt", len(reqs))
	}
	req := reqs[0]
	if len(req.Body.ToGroups) != 0 || len(req.Body.ToUsers) != 1 ||
		string(req.Body.ToUsers[0]) != `"carol"` {
		t.Errorf("prompt targets = users %v groups %v, want private tousers only",
			req.Body.ToUsers, req.Body.ToGroups)
	}
	// TrimRight on AppURL must not double the slash before the path.
	if want := "[link your account](https://app.test/tuitui/bind?token=tok-123)"; !strings.Contains(req.Body.Text.Content, want) {
		t.Errorf("prompt text = %q, want it to contain %q", req.Body.Text.Content, want)
	}
}

func TestTuituiReplierWithoutMinterStaysSilent(t *testing.T) {
	srv, stub, port := newSendStub(t)
	r := NewOutboundReplier(OutboundReplierConfig{
		HTTPClient: srv.Client(),
		AppURL:     "https://app.test",
		Logger:     testDiscardLogger(),
	})
	inst := replierTestInstallation(t, "127.0.0.1", port)
	msg := channel.InboundMessage{Source: channel.Source{
		ChatID: "alice", ChatType: channel.ChatTypeP2P, SenderID: "alice",
	}}
	// No binding service configured: the prompt is skipped (logged), never
	// a half-link, and the status notices still work.
	r.Reply(context.Background(), inst, msg, engine.Result{Outcome: engine.OutcomeNeedsBinding, Sender: "alice"})
	if reqs := stub.all(); len(reqs) != 0 {
		t.Errorf("sends = %d, want 0 without a minter", len(reqs))
	}
	r.Reply(context.Background(), inst, msg, engine.Result{Outcome: engine.OutcomeAgentArchived})
	if reqs := stub.all(); len(reqs) != 1 {
		t.Errorf("sends = %d, want the archived notice to still fire", len(reqs))
	}
}
