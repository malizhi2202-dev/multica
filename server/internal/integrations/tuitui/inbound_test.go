package tuitui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// frameJSON builds one callback frame body for the parsers under test.
func frameJSON(t *testing.T, event string, data string) *wsFrame {
	t.Helper()
	raw := `{"event_id":"ev-1","body":{"event":"` + event +
		`","user_account":"alice","user_name":"Alice","data":` + data + `}}`
	var f wsFrame
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return &f
}

func TestNormalizeSingleChat(t *testing.T) {
	f := frameJSON(t, "single_chat", `{"msgid":"m-1","msg_type":"text","text":"hello"}`)
	n, ok := normalizeEvent(f, "app-1")
	if !ok {
		t.Fatal("single_chat should normalize")
	}
	msg := n.msg
	if n.kind != kindP2P {
		t.Errorf("kind = %d, want kindP2P", n.kind)
	}
	if msg.Source.ChatID != "alice" || msg.Source.ChatType != channel.ChatTypeP2P {
		t.Errorf("source = %+v", msg.Source)
	}
	if msg.MessageID != "m-1" || msg.EventID != "ev-1" {
		t.Errorf("ids = %q / %q", msg.MessageID, msg.EventID)
	}
	if msg.Text != "hello" || msg.Type != channel.MsgTypeText {
		t.Errorf("text/type = %q / %v", msg.Text, msg.Type)
	}
	if !msg.AddressedToBot {
		t.Error("p2p must be addressed to bot")
	}
	if msg.Source.ChannelType != TypeTuitui {
		t.Errorf("channel type = %q", msg.Source.ChannelType)
	}
}

func TestNormalizeGroupChatAddressed(t *testing.T) {
	cases := []struct {
		name  string
		data  string
		want  bool
		reply string
		ctx   bool
	}{
		{
			name: "unknown_fields_ignored",
			data: `{"msgid":"g-1","group_id":"1234567890123456","msg_type":"text","text":"hi","at_me":true}`,
			want: false,
		},
		{
			name: "reply_to_bot",
			data: `{"msgid":"g-1","group_id":"1234567890123456","msg_type":"text","text":"hi","ref":{"msgid":"b-1","is_me":true,"content":"prior"}}`,
			want: true, reply: "b-1", ctx: true,
		},
		{
			name: "reply_to_other_no_reply_ctx",
			data: `{"msgid":"g-1","group_id":"1234567890123456","msg_type":"text","text":"hi","ref":{"msgid":"x-9","is_me":false,"content":"theirs"}}`,
			want: false, ctx: true,
		},
		{
			name: "silent",
			data: `{"msgid":"g-1","group_id":"1234567890123456","msg_type":"text","text":"hi"}`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := frameJSON(t, "group_chat", tc.data)
			n, ok := normalizeEvent(f, "app-1")
			if !ok {
				t.Fatal("group_chat should normalize")
			}
			if n.kind != kindGroup {
				t.Errorf("kind = %d, want kindGroup", n.kind)
			}
			if n.msg.Source.ChatID != "1234567890123456" || n.msg.Source.ChatType != channel.ChatTypeGroup {
				t.Errorf("source = %+v", n.msg.Source)
			}
			if n.msg.AddressedToBot != tc.want {
				t.Errorf("AddressedToBot = %v, want %v", n.msg.AddressedToBot, tc.want)
			}
			switch {
			case tc.reply == "" && n.msg.ReplyTo != nil:
				t.Errorf("unexpected ReplyTo %+v", n.msg.ReplyTo)
			case tc.reply != "":
				if n.msg.ReplyTo == nil || n.msg.ReplyTo.MessageID != tc.reply {
					t.Fatalf("ReplyTo = %+v, want msgid %q", n.msg.ReplyTo, tc.reply)
				}
			}
			if n.msg.HasSelectedContext != tc.ctx {
				t.Errorf("HasSelectedContext = %v, want %v", n.msg.HasSelectedContext, tc.ctx)
			}
		})
	}
}

func TestNormalizeTeamsCompositeChatID(t *testing.T) {
	t.Run("thread reply", func(t *testing.T) {
		f := frameJSON(t, "teams_post_create", `{"team_id":"t1","channel_id":"c2","post_id":"p9","parent_id":"th3","content":"yes"}`)
		n, ok := normalizeEvent(f, "app-1")
		if !ok {
			t.Fatal("teams_post_create should normalize")
		}
		if n.kind != kindTeams {
			t.Errorf("kind = %d, want kindTeams", n.kind)
		}
		if got := n.msg.Source.ChatID; got != "teams_t1_c2_th3" {
			t.Errorf("ChatID = %q, want teams_t1_c2_th3", got)
		}
		if n.msg.Source.ChatType != channel.ChatTypeGroup {
			t.Errorf("ChatType = %q, want group (closed enum)", n.msg.Source.ChatType)
		}
		if n.msg.Source.ThreadID != "th3" {
			t.Errorf("ThreadID = %q, want th3", n.msg.Source.ThreadID)
		}
		if n.msg.MessageID != "p9" {
			t.Errorf("MessageID = %q, want p9", n.msg.MessageID)
		}
	})
	t.Run("root post falls back to post_id as thread", func(t *testing.T) {
		f := frameJSON(t, "teams_post_modify", `{"team_id":7,"channel_id":8,"post_id":99,"parent_id":"0","content":"edit"}`)
		n, ok := normalizeEvent(f, "app-1")
		if !ok {
			t.Fatal("numeric ids must normalize")
		}
		if got := n.msg.Source.ChatID; got != "teams_7_8_99" {
			t.Errorf("ChatID = %q, want teams_7_8_99", got)
		}
	})
}

func TestNormalizeDropsWithoutPlatformID(t *testing.T) {
	// No msgid, no post_id: channel_inbound_message_dedup depends on a real
	// platform id, so these must be dropped (never uuid-minted like the
	// reference client did).
	f := frameJSON(t, "single_chat", `{"msg_type":"text","text":"anon"}`)
	if _, ok := normalizeEvent(f, "app-1"); ok {
		t.Fatal("message without msgid must be dropped")
	}
	f = frameJSON(t, "teams_post_create", `{"team_id":"t","channel_id":"c","content":"no id"}`)
	if _, ok := normalizeEvent(f, "app-1"); ok {
		t.Fatal("teams post without post_id must be dropped")
	}
	f = frameJSON(t, "someone_elses_event", `{"msgid":"m"}`)
	if _, ok := normalizeEvent(f, "app-1"); ok {
		t.Fatal("unknown event type must be dropped")
	}
}

func TestNormalizeMediaPlaceholders(t *testing.T) {
	cases := []struct {
		name  string
		event string
		data  string
		// wantText is the exact durable body; no case may carry a media url.
		wantText string
		// wantSources are the download instructions the MediaResolver reads
		// back out of Raw, in platform order.
		wantSources []tuituiMediaSource
		wantType    channel.MsgType
	}{
		{
			name:     "only text",
			event:    eventSingleChat,
			data:     `{"msgid":"m","msg_type":"text","text":"plain"}`,
			wantText: "plain", wantType: channel.MsgTypeText,
		},
		{
			name:     "multiple images",
			event:    eventSingleChat,
			data:     `{"msgid":"m","msg_type":"image","images":["u1","u2"]}`,
			wantText: "[图片]\n[图片]", wantType: channel.MsgTypeImage,
			wantSources: []tuituiMediaSource{
				{URL: "u1", Kind: channel.MsgTypeImage},
				{URL: "u2", Kind: channel.MsgTypeImage},
			},
		},
		{
			name:     "image url keeps its signature query string",
			event:    eventSingleChat,
			data:     `{"msgid":"m","msg_type":"image","images":["https://media.test/a.png?sig=topsecret&t=2"]}`,
			wantText: "[图片]", wantType: channel.MsgTypeImage,
			wantSources: []tuituiMediaSource{
				{URL: "https://media.test/a.png?sig=topsecret&t=2", Kind: channel.MsgTypeImage},
			},
		},
		{
			name:     "mixed with text keeps the text alone and still carries media",
			event:    eventSingleChat,
			data:     `{"msgid":"m","msg_type":"mixed","text":"看图","images":["u1"]}`,
			wantText: "看图", wantType: channel.MsgTypeText,
			wantSources: []tuituiMediaSource{{URL: "u1", Kind: channel.MsgTypeImage}},
		},
		{
			name:     "malformed image entries without a url are skipped",
			event:    eventSingleChat,
			data:     `{"msgid":"m","msg_type":"image","images":["",{"name":"no-url"},"u3"]}`,
			wantText: "[图片]", wantType: channel.MsgTypeImage,
			wantSources: []tuituiMediaSource{{URL: "u3", Kind: channel.MsgTypeImage}},
		},
		{
			name:     "voice and video carry one url each",
			event:    eventSingleChat,
			data:     `{"msgid":"m","msg_type":"video","video":"https://media.test/v.mp4?sig=x"}`,
			wantText: "[视频]", wantType: channel.MsgTypeVideo,
			wantSources: []tuituiMediaSource{{URL: "https://media.test/v.mp4?sig=x", Kind: channel.MsgTypeVideo}},
		},
		{
			name:     "file names the attachment without the url",
			event:    eventSingleChat,
			data:     `{"msgid":"m","msg_type":"file","file":{"url":"https://media.test/f.bin?sig=y","name":"report.bin"}}`,
			wantText: "[文件] report.bin", wantType: channel.MsgTypeFile,
			wantSources: []tuituiMediaSource{{URL: "https://media.test/f.bin?sig=y", Kind: channel.MsgTypeFile, Filename: "report.bin"}},
		},
		{
			name:  "teams post mixes images and files in order",
			event: eventTeamsPostCreate,
			data: `{"team_id":"t","channel_id":"c","post_id":"p","content":"look",` +
				`"images":[{"url":"i1","name":"pic.png"},{"name":"malformed-no-url"},"str-item"],` +
				`"files":[{"url":"f1"},{"url":""}]}`,
			wantText: "look\n[图片]\n[图片]\n[文件] unknown", wantType: channel.MsgTypeText,
			wantSources: []tuituiMediaSource{
				{URL: "i1", Kind: channel.MsgTypeImage, Filename: "pic.png"},
				{URL: "str-item", Kind: channel.MsgTypeImage},
				{URL: "f1", Kind: channel.MsgTypeFile},
			},
		},
		{
			name:     "teams contentless post with media keeps a placeholder",
			event:    eventTeamsPostCreate,
			data:     `{"team_id":"t","channel_id":"c","post_id":"p","content":"","images":[{"url":"i1"}]}`,
			wantText: "\n[图片]", wantType: channel.MsgTypeText,
			wantSources: []tuituiMediaSource{{URL: "i1", Kind: channel.MsgTypeImage}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := frameJSON(t, tc.event, tc.data)
			n, ok := normalizeEvent(f, "app-1")
			if !ok {
				t.Fatalf("frame should normalize: %s", tc.data)
			}
			msg := n.msg
			if msg.Text != tc.wantText {
				t.Errorf("Text = %q, want %q", msg.Text, tc.wantText)
			}
			if msg.Type != tc.wantType {
				t.Errorf("Type = %v, want %v", msg.Type, tc.wantType)
			}
			// The core invariant: MediaRefs is the resolver's output, never
			// adapter-populated at normalization time.
			if len(msg.MediaRefs) != 0 {
				t.Errorf("MediaRefs = %+v, want empty at normalize time", msg.MediaRefs)
			}
			// The download instructions must survive verbatim in Raw — this is
			// exactly what engine.MediaResolver resolves after the message is
			// durable.
			got := tuituiMediaSources(msg)
			if len(got) != len(tc.wantSources) {
				t.Fatalf("media sources = %+v, want %+v", got, tc.wantSources)
			}
			for i := range got {
				if got[i] != tc.wantSources[i] {
					t.Errorf("source %d = %+v, want %+v", i, got[i], tc.wantSources[i])
				}
			}
			// No raw media url may appear in the durable body, or the bound
			// attachment would double it there.
			for _, s := range tc.wantSources {
				if s.URL != "" && strings.Contains(msg.Text, s.URL) {
					t.Errorf("Text %q leaks media url %q", msg.Text, s.URL)
				}
			}
		})
	}
}

func TestNormalizeMediaOnlyDegradesWithoutURLs(t *testing.T) {
	// Malformed entries only: the message still normalizes (it was addressed
	// text-less media), but nothing is downloadable.
	f := frameJSON(t, eventSingleChat, `{"msgid":"m","msg_type":"image","images":[{"name":"x"}]}`)
	n, ok := normalizeEvent(f, "app-1")
	if !ok {
		t.Fatal("image frame with malformed entries should still normalize")
	}
	if len(tuituiMediaSources(n.msg)) != 0 {
		t.Errorf("sources = %+v, want none", tuituiMediaSources(n.msg))
	}

	// A voice msg_type whose url is empty keeps the reference's bare
	// placeholder but offers the resolver nothing.
	f = frameJSON(t, eventSingleChat, `{"msgid":"m2","msg_type":"voice","voice":""}`)
	n, ok = normalizeEvent(f, "app-1")
	if !ok || n.msg.Text != "[语音]" || len(tuituiMediaSources(n.msg)) != 0 {
		t.Errorf("empty voice = %+v ok=%v", n.msg, ok)
	}
}

func TestNormalizeDropsFrameWithoutMsgData(t *testing.T) {
	// A frame with no data payload has no platform message id: hard drop
	// (dedup keys on it), never a uuid-minted phantom like the reference.
	raw := `{"event_id":"ev-1","body":{"event":"single_chat","user_account":"alice","user_name":"Alice"}}`
	var f wsFrame
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if _, ok := normalizeEvent(&f, "app-1"); ok {
		t.Fatal("frame without msg_data must be dropped")
	}
}

func TestSplitComposeTeamsChatID(t *testing.T) {
	team, ch, thread, ok := splitTeamsChatID("teams_t1_c2_th3")
	if !ok || team != "t1" || ch != "c2" || thread != "th3" {
		t.Fatalf("split composite = %q %q %q %v", team, ch, thread, ok)
	}
	if _, _, _, ok := splitTeamsChatID("1234567890123456"); ok {
		t.Error("plain group id must not split as teams")
	}
	if _, _, _, ok := splitTeamsChatID("teams_onlyteam"); ok {
		t.Error("incomplete teams id must fail to split")
	}
	// Embedded underscores make the composite ambiguous (platform ids must
	// not contain underscores): 4+ segments is a hard failure, not a guess.
	if _, _, _, ok := splitTeamsChatID("teams_a_b_c_d"); ok {
		t.Error("teams id with an embedded underscore must fail to split")
	}
	if _, _, _, ok := splitTeamsChatID("teams__c2"); ok {
		t.Error("empty team segment must fail to split")
	}
	if got := composeTeamsChatID("t", "c", ""); got != "teams_t_c" {
		t.Errorf("compose without thread = %q", got)
	}
}

func TestNormalizeTeamsAddressedToBot(t *testing.T) {
	// Teams posts carry no mention / is_me signal; the bot is the reason
	// the channel subscription exists, so posts arrive as addressed.
	f := frameJSON(t, "teams_post_create", `{"team_id":"t","channel_id":"c","post_id":"p","content":"hi"}`)
	n, ok := normalizeEvent(f, "app-1")
	if !ok || !n.msg.AddressedToBot {
		t.Fatalf("teams post must normalize as addressed: %+v ok=%v", n.msg, ok)
	}
	// Modify is one of the four deliverable events (dedup of the
	// create/modify pair is the core's job, not the adapter's).
	f = frameJSON(t, "teams_post_modify", `{"team_id":"t","channel_id":"c","post_id":"p","content":"edited"}`)
	if n2, ok := normalizeEvent(f, "app-1"); !ok || n2.msg.MessageID != "p" {
		t.Fatalf("teams_post_modify must normalize, ok=%v %+v", ok, n2.msg)
	}
}

func TestLooksLikeCardCallback(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
	}{
		{"data.message callback", `{"message":{"msgid":"m1","action":[{"value":"approve"}],"user":{"account":"u"}}}`, true},
		{"single dict action", `{"message":{"msgid":"m1","action":{"value":"x"}}}`, true},
		{"legacy flat on data", `{"msgid":"m1","action":{"value":"x"}}`, true},
		{"empty action", `{"message":{"msgid":"m1","action":[]}}`, false},
		{"no msgid", `{"message":{"action":[{"v":1}]}}`, false},
		{"ordinary chat payload", `{"msgid":"m1","msg_type":"text","text":"hello"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := eventBody{Event: "whatever", Data: json.RawMessage(tc.data)}
			if got := looksLikeCardCallback(&body); got != tc.want {
				t.Errorf("looksLikeCardCallback = %v, want %v", got, tc.want)
			}
		})
	}
	// Flattened shape: the card sits directly under body.message.
	body := eventBody{Event: "card_click", Message: json.RawMessage(`{"msgid":"m2","action":[{"v":1}]}`)}
	if !looksLikeCardCallback(&body) {
		t.Error("body.message flattened callback must be detected")
	}
}
