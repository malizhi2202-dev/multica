package tuitui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// stubAPI captures the last request and replies with a canned body.
type stubAPI struct {
	t        *testing.T
	lastPath string
	lastBody sendPayload
	reply    string
	status   int
	err      error
}

func newStubChannel(t *testing.T, reply string) (*tuituiChannel, *httptest.Server, func() (string, sendPayload)) {
	t.Helper()
	var gotPath string
	var gotBody sendPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("appid") != "app-1" || q.Get("secret") != "sec-1" {
			t.Errorf("missing query auth: %v", q)
		}
		body, _ := io.ReadAll(r.Body)
		var p sendPayload
		if err := json.Unmarshal(body, &p); err != nil {
			t.Errorf("decode payload: %v (%s)", err, body)
		}
		gotPath = r.URL.Path
		gotBody = p
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	cfg := Config{
		AppID: "app-1", AppSecret: "sec-1",
		Host: srv.URL, Port: 0, MaxMessageLength: DefaultMaxMessageLength,
		ReactionEmoji: DefaultReactionEmoji,
	}
	ch := New(cfg, nil, ConfigDeps{HTTPClient: srv.Client()})
	// The stub server is http:// and carries its own port: point the client
	// straight at it instead of the https://{host}:{port} builder.
	ch.client.baseURL = srv.URL
	return ch, srv, func() (string, sendPayload) { return gotPath, gotBody }
}

func baseConfig() Config {
	return Config{AppID: "app-1", AppSecret: "sec-1", Host: DefaultHost, Port: DefaultPort,
		MaxMessageLength: DefaultMaxMessageLength, ReactionEmoji: DefaultReactionEmoji}
}

func TestSendP2PTextPayload(t *testing.T) {
	ch, srv, got := newStubChannel(t, `{"errcode":0,"msgid":"msg-9"}`)
	defer srv.Close()
	ch.rememberKind("alice", kindP2P)

	res, err := ch.Send(context.Background(), channel.OutboundMessage{ChatID: "alice", Text: "hi there"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.MessageID != "msg-9" || len(res.MessageIDs) != 1 || res.MessageIDs[0] != "msg-9" {
		t.Errorf("SendResult = %+v", res)
	}
	path, p := got()
	if !strings.HasSuffix(path, pathSend) {
		t.Errorf("path = %s", path)
	}
	if p.MsgType != "text" || p.Text == nil || p.Text.Content != "hi there" {
		t.Errorf("payload = %+v", p)
	}
	if len(p.ToUsers) != 1 || string(p.ToUsers[0]) != `"alice"` || p.ToGroups != nil || p.ToTeams != nil {
		t.Errorf("targets = users %s groups %s teams %+v", p.ToUsers, p.ToGroups, p.ToTeams)
	}
}

func TestSendGroupTextPayload(t *testing.T) {
	ch, srv, got := newStubChannel(t, `{"errcode":0,"msgids":[{"msgid":"m-a"},{"msgid":"m-b"}]}`)
	defer srv.Close()
	ch.rememberKind("1234567890123456", kindGroup)

	res, err := ch.Send(context.Background(), channel.OutboundMessage{ChatID: "1234567890123456", Text: "hello group"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.MessageID != "m-a" {
		t.Errorf("MessageID = %q, want m-a", res.MessageID)
	}
	_, p := got()
	if len(p.ToGroups) != 1 || string(p.ToGroups[0]) != `"1234567890123456"` || p.ToUsers != nil {
		t.Errorf("targets = groups %s users %s", p.ToGroups, p.ToUsers)
	}
}

func TestSendTeamsMarkdownPayload(t *testing.T) {
	ch, srv, got := newStubChannel(t, `{"errcode":0,"msgids":["str-id"]}`)
	defer srv.Close()
	ch.rememberKind("teams_t1_c2_th3", kindTeams)

	res, err := ch.Send(context.Background(), channel.OutboundMessage{ChatID: "teams_t1_c2_th3", Text: "# answer"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.MessageID != "str-id" {
		t.Errorf("MessageID = %q, want str-id (bare-string msgids shape)", res.MessageID)
	}
	_, p := got()
	if p.MsgType != "richtext/markdown" || p.RichText == nil || p.RichText.Markdown != "# answer" {
		t.Fatalf("payload = %+v", p)
	}
	if len(p.ToTeams) != 1 {
		t.Fatalf("toteams = %+v", p.ToTeams)
	}
	tt := p.ToTeams[0]
	if tt.TeamID != "t1" || tt.ChannelID != "c2" || tt.ParentID != "th3" {
		t.Errorf("toteams triple = %+v", tt)
	}
	if p.ToUsers != nil || p.ToGroups != nil {
		t.Error("teams send must not carry user/group targets")
	}
}

func TestSendTeamsMalformedChatIDFails(t *testing.T) {
	ch, srv, _ := newStubChannel(t, `{"errcode":0}`)
	defer srv.Close()
	// 1 segment: no channel id.
	if _, err := ch.Send(context.Background(), channel.OutboundMessage{ChatID: "teams_broken", Text: "x"}); err == nil {
		t.Fatal("teams send with an unsplittable chat id must return an error")
	}
	// 4 segments: one of the ids contains an underscore, the composite is
	// ambiguous, and Send must fail rather than guess a recipient.
	if _, err := ch.Send(context.Background(), channel.OutboundMessage{ChatID: "teams_a_b_c_d", Text: "x"}); err == nil {
		t.Fatal("teams chat id with an embedded underscore must fail to reverse-parse")
	}
	// Empty team or channel segment.
	if _, err := ch.Send(context.Background(), channel.OutboundMessage{ChatID: "teams__c2_th", Text: "x"}); err == nil {
		t.Fatal("teams chat id with an empty segment must fail to reverse-parse")
	}
}

// Cold kind map (simulates a process restart: nothing remembered from
// inbound) must still route correctly — teams purely by ChatID prefix,
// plain chats by the platform ID-format backstop.
func TestSendRoutesColdWithoutKindMap(t *testing.T) {
	t.Run("teams prefix → markdown even with empty map", func(t *testing.T) {
		ch, srv, got := newStubChannel(t, `{"errcode":0,"msgid":"t-1"}`)
		defer srv.Close()
		if _, err := ch.Send(context.Background(), channel.OutboundMessage{ChatID: "teams_t9_c8_p7", Text: "md"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		_, p := got()
		if p.MsgType != "richtext/markdown" || len(p.ToTeams) != 1 ||
			p.ToTeams[0].TeamID != "t9" || p.ToTeams[0].ChannelID != "c8" || p.ToTeams[0].ParentID != "p7" {
			t.Errorf("cold teams payload = %+v", p)
		}
	})
	t.Run("numeric id → togroups", func(t *testing.T) {
		ch, srv, got := newStubChannel(t, `{"errcode":0,"msgid":"g-1"}`)
		defer srv.Close()
		if _, err := ch.Send(context.Background(), channel.OutboundMessage{ChatID: "1234567890123456", Text: "hi"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		_, p := got()
		if len(p.ToGroups) != 1 || p.ToUsers != nil {
			t.Errorf("cold numeric-id payload users=%s groups=%s", p.ToUsers, p.ToGroups)
		}
	})
	t.Run("account id → tousers", func(t *testing.T) {
		ch, srv, got := newStubChannel(t, `{"errcode":0,"msgid":"d-1"}`)
		defer srv.Close()
		if _, err := ch.Send(context.Background(), channel.OutboundMessage{ChatID: "alice", Text: "hi"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		_, p := got()
		if len(p.ToUsers) != 1 || p.ToGroups != nil {
			t.Errorf("cold account-id payload users=%s groups=%s", p.ToUsers, p.ToGroups)
		}
	})
	t.Run("empty chat id → error", func(t *testing.T) {
		ch, srv, _ := newStubChannel(t, `{"errcode":0}`)
		defer srv.Close()
		if _, err := ch.Send(context.Background(), channel.OutboundMessage{ChatID: "  ", Text: "hi"}); err == nil {
			t.Error("Send with an empty ChatID must fail")
		}
	})
}

func TestSendErrcodeNonZero(t *testing.T) {
	ch, srv, _ := newStubChannel(t, `{"errcode":40004,"errmsg":"bad group"}`)
	defer srv.Close()
	ch.rememberKind("alice", kindP2P)
	_, err := ch.Send(context.Background(), channel.OutboundMessage{ChatID: "alice", Text: "x"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.ErrCode != 40004 || !strings.Contains(err.Error(), "bad group") {
		t.Errorf("APIError = %+v (%v)", apiErr, err)
	}
}

func TestSendLongMessageChunked(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p sendPayload
		_ = json.Unmarshal(body, &p)
		bodies = append(bodies, p.Text.Content)
		_, _ = io.WriteString(w, `{"errcode":0,"msgid":"c`+string(rune('0'+len(bodies)))+`"}`)
	}))
	defer srv.Close()
	cfg := baseConfig()
	cfg.MaxMessageLength = 10
	ch := New(cfg, nil, ConfigDeps{HTTPClient: srv.Client()})
	ch.client.baseURL = srv.URL
	ch.rememberKind("alice", kindP2P)

	text := strings.Repeat("aaaa\n", 6) // 30 chars incl. newlines, hard to fit in 10
	res, err := ch.Send(context.Background(), channel.OutboundMessage{ChatID: "alice", Text: text})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(bodies) < 2 {
		t.Fatalf("expected multiple chunks, got %d: %q", len(bodies), bodies)
	}
	for _, b := range bodies {
		if len([]rune(b)) > 10 {
			t.Errorf("chunk exceeds cap: %d %q", len([]rune(b)), b)
		}
	}
	if joined := strings.Join(bodies, ""); joined != text {
		t.Errorf("chunks lost/altered content: %q", joined)
	}
	if len(res.MessageIDs) != len(bodies) {
		t.Errorf("MessageIDs = %v, want one per chunk", res.MessageIDs)
	}
	if want := string(rune('0' + len(bodies))); res.MessageID != "c"+want {
		t.Errorf("MessageID = %q, want last chunk id c%s", res.MessageID, want)
	}
}

func TestSplitChunks(t *testing.T) {
	if got := splitChunks("short", 10); len(got) != 1 || got[0] != "short" {
		t.Errorf("short passthrough = %q", got)
	}
	got := splitChunks("para one\n\npara two\n\npara three", 10)
	if len(got) < 2 {
		t.Fatalf("paragraph split = %q", got)
	}
	if got[0] != "para one\n\n" {
		t.Errorf("first chunk = %q", got[0])
	}
	joined := strings.ReplaceAll(strings.Join(got, ""), "\n", "")
	if joined != strings.ReplaceAll("para one\n\npara two\n\npara three", "\n", "") {
		t.Errorf("chunks lost content: %q", got)
	}
}

func TestInteractiveSendAndModify(t *testing.T) {
	ch, srv, got := newStubChannel(t, `{"errcode":0,"msgid":"card-1"}`)
	defer srv.Close()
	ch.rememberKind("alice", kindP2P)

	card := json.RawMessage(`{"title":"t","elements":[]}`)
	id, err := ch.SendInteractive(context.Background(), "alice", card)
	if err != nil {
		t.Fatalf("SendInteractive: %v", err)
	}
	if id != "card-1" {
		t.Errorf("id = %q", id)
	}
	_, p := got()
	if p.MsgType != "interactive" || string(p.Interactive) != `{"title":"t","elements":[]}` {
		t.Errorf("interactive payload = %+v", p)
	}
	if len(p.ToUsers) != 1 || string(p.ToUsers[0]) != `"alice"` {
		t.Errorf("targets = %s", p.ToUsers)
	}

	if err := ch.UpdateInteractive(context.Background(), "alice", "card-1", card); err != nil {
		t.Fatalf("UpdateInteractive: %v", err)
	}
	path, p := got()
	if !strings.HasSuffix(path, pathModify) {
		t.Errorf("modify path = %s", path)
	}
	if len(p.ToUsers) != 1 || !strings.Contains(string(p.ToUsers[0]), `"msgid":"card-1"`) {
		t.Errorf("modify target = %s", p.ToUsers)
	}

	// Teams chats reject cards, both send and modify — by ChatID prefix
	// alone (no kind-map warming), so the rejection survives a restart.
	if _, err := ch.SendInteractive(context.Background(), "teams_t_c_th", card); err == nil {
		t.Error("teams interactive send must fail")
	}
	if err := ch.UpdateInteractive(context.Background(), "teams_t_c_th", "x", card); err == nil {
		t.Error("teams interactive modify must fail")
	}
}

func TestSendReactionPayload(t *testing.T) {
	ch, srv, got := newStubChannel(t, `{"errcode":0}`)
	defer srv.Close()
	ch.rememberKind("grp-1", kindGroup)
	if err := ch.SendReaction(context.Background(), "grp-1", "m-5", ""); err != nil {
		t.Fatalf("SendReaction: %v", err)
	}
	_, p := got()
	if p.MsgType != "emoji_reaction" || p.Reaction == nil || p.Reaction.Emoji != DefaultReactionEmoji {
		t.Fatalf("reaction payload = %+v", p)
	}
	if len(p.ToGroups) != 1 || !strings.Contains(string(p.ToGroups[0]), `"msgid":"m-5"`) {
		t.Errorf("reaction target = %s", p.ToGroups)
	}
}

func TestUploadAndFileSend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, pathUpload) {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("multipart: %v", err)
			}
			if r.FormValue("type") != "file" {
				t.Errorf("upload type = %q", r.FormValue("type"))
			}
			_, _ = io.WriteString(w, `{"errcode":0,"data":{"media_id":"fid-7"}}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"errcode":0,"msgid":"f-1"}`)
		_ = body
	}))
	defer srv.Close()
	cfg := baseConfig()
	ch := New(cfg, nil, ConfigDeps{HTTPClient: srv.Client()})
	ch.client.baseURL = srv.URL
	ch.rememberKind("alice", kindP2P)

	fid, err := ch.UploadMedia(context.Background(), "file", "a.txt", []byte("hello"))
	if err != nil || fid != "fid-7" {
		t.Fatalf("UploadMedia = %q, %v", fid, err)
	}
	id, err := ch.SendFile(context.Background(), "alice", "file", "a.txt", []byte("hello"))
	if err != nil || id != "f-1" {
		t.Fatalf("SendFile = %q, %v", id, err)
	}
}

func TestCapabilitiesStable(t *testing.T) {
	ch := New(baseConfig(), nil, ConfigDeps{})
	want := channel.CapText | channel.CapRichCard | channel.CapThreadReply |
		channel.CapMessageEdit | channel.CapAttachment
	for i := 0; i < 3; i++ {
		if got := ch.Capabilities(); got != want {
			t.Fatalf("Capabilities = %v, want %v", got, want)
		}
	}
	if ch.Type() != channel.Type("tuitui") {
		t.Errorf("Type = %q", ch.Type())
	}
}
