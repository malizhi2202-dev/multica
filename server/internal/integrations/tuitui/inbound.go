package tuitui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// Inbound WS frame event types (reference client._parse_event).
const (
	eventSingleChat        = "single_chat"
	eventGroupChat         = "group_chat"
	eventTeamsPostCreate   = "teams_post_create"
	eventTeamsPostModify   = "teams_post_modify"
	eventKeepalive         = "keepalive"
	eventInteractiveAction = "interactive_action"
)

// chatKind is the adapter's classification of a conversation, used to pick
// the outbound payload shape (tousers vs togroups vs toteams). The channel
// ChatType set is closed (p2p|group) and teams conversations map onto
// ChatTypeGroup, so the shape cannot be recovered from ChatType alone.
//
// Routing rules, in order of authority (see tuituiChannel.routeKind):
//   - teams is decided ONLY by the "teams_" ChatID prefix, never by any map
//     or heuristic, so it holds across process restarts;
//   - plain p2p vs group prefers the true event kind recorded by the receive
//     loop from the real event_type; after a restart that memory is empty and
//     a platform ID-format backstop applies (group ids are pure-numeric, user
//     accounts are not — the reference client's own ID format fact).
type chatKind int

const (
	kindUnknown chatKind = iota
	kindP2P
	kindGroup
	kindTeams
)

// wsFrame is one callback frame on the inbound WebSocket. Every frame with an
// event_id must be acknowledged with {"ack": event_id} or the gateway keeps
// redelivering it.
type wsFrame struct {
	EventID json.RawMessage `json:"event_id"`
	Body    eventBody       `json:"body"`
}

// eventID renders the frame's event id whether the platform sent it as a
// string or a number; "" means an unnumbered frame (never acked, dropped).
func (f *wsFrame) eventID() string { return rawID(f.EventID) }

type eventBody struct {
	Event       string          `json:"event"`
	UserAccount string          `json:"user_account"`
	UserName    string          `json:"user_name"`
	Data        json.RawMessage `json:"data"`
	// Message holds the interactive-callback card in the flattened shape
	// some platform builds use instead of data.message.
	Message json.RawMessage `json:"message"`
}

// cardProbe matches the interactive-callback card body in any of the three
// locations the reference client probes (body.data.message, body.message,
// then the flattened data itself).
type cardProbe struct {
	Action json.RawMessage `json:"action"`
	MsgID  flexString      `json:"msgid"`
}

// looksLikeCardCallback reports whether the event body is a card-button
// callback (interactive_action). Such frames must be acknowledged and
// dropped, never delivered to the core as chat messages. Detection mirrors
// reference client._parse_interactive_callback: a non-empty action plus a
// msgid on the card body.
func looksLikeCardCallback(body *eventBody) bool {
	if len(body.Data) > 0 {
		var d struct {
			Message *cardProbe      `json:"message"`
			Action  json.RawMessage `json:"action"`
			MsgID   flexString      `json:"msgid"`
		}
		if json.Unmarshal(body.Data, &d) == nil {
			if d.Message != nil && d.Message.hasAction() && d.Message.MsgID != "" {
				return true
			}
			// Legacy flat shape: the callback fields live on data itself.
			if d.Action != nil && nonEmptyAction(d.Action) && d.MsgID != "" {
				return true
			}
		}
	}
	if len(body.Message) > 0 {
		var m cardProbe
		if json.Unmarshal(body.Message, &m) == nil && m.hasAction() && m.MsgID != "" {
			return true
		}
	}
	return false
}

func (c *cardProbe) hasAction() bool { return c != nil && nonEmptyAction(c.Action) }

// nonEmptyAction accepts the action field in both shapes the platform
// sends: a non-empty array or a single object (the reference client wraps
// the dict form into a one-element list).
func nonEmptyAction(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	switch t {
	case "", "null", "[]", "{}", "false", "0", `""`:
		return false
	}
	return true
}

// eventPayload is the union of data fields across the four chat events.
// ids arrive as JSON strings or numbers depending on the event, hence the
// flex types.
type eventPayload struct {
	// Common message fields.
	MsgID   string     `json:"msgid"`
	MsgType string     `json:"msg_type"`
	Text    string     `json:"text"`
	Ref     *refInfo   `json:"ref"`
	Images  []flexPart `json:"images"`
	Files   []flexPart `json:"files"`
	Voice   string     `json:"voice"`
	Video   string     `json:"video"`
	File    filePart   `json:"file"`
	Link    linkPart   `json:"link"`

	// group_chat.
	GroupID   flexString `json:"group_id"`
	GroupName string     `json:"group_name"`

	// teams_post_create / teams_post_modify.
	TeamID      flexString `json:"team_id"`
	ChannelID   flexString `json:"channel_id"`
	ChannelName string     `json:"channel_name"`
	PostID      flexString `json:"post_id"`
	ParentID    flexString `json:"parent_id"`
	Content     string     `json:"content"`
}

type refInfo struct {
	MsgID    flexString `json:"msgid"`
	Content  string     `json:"content"`
	UserName string     `json:"user_name"`
	IsMe     bool       `json:"is_me"`
}

// flexPart covers both media shapes: plain-chat events carry image url
// strings, teams posts carry {url, name} objects.
type flexPart struct {
	URL  string
	Name string
}

func (p *flexPart) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if trimmed[0] == '"' {
		return json.Unmarshal(b, &p.URL)
	}
	var obj struct {
		URL  flexString `json:"url"`
		Name string     `json:"name"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	p.URL, p.Name = obj.URL.String(), obj.Name
	return nil
}

type filePart struct {
	URL  string `json:"url"`
	Name string `json:"name"`
}

type linkPart struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// flexString accepts a JSON string or number as text.
type flexString string

func (s flexString) String() string { return string(s) }

func (s *flexString) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		*s = ""
		return nil
	}
	if trimmed[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		*s = flexString(str)
		return nil
	}
	var num json.Number
	if err := json.Unmarshal(b, &num); err != nil {
		return err
	}
	*s = flexString(num.String())
	return nil
}

// normalizedInbound is the adapter-internal result of translating one chat
// event: the shared envelope plus the platform kind of the conversation.
type normalizedInbound struct {
	msg  channel.InboundMessage
	kind chatKind
}

// normalizeEvent translates one inbound chat event into the shared
// InboundMessage shape. It returns ok=false for frames that must not reach
// the core: unknown events, events without a usable platform message id
// (channel_inbound_message_dedup keys on it, and the reference uuid fallback
// would mint a new id for every redelivery), or events without a chat id.
//
// The fourth event_type is deliberately explicit (no string-shape guessing):
// single_chat → p2p, group_chat → group, and both teams_post_* events →
// group with a composite teams chat id, because channel.ChatType is closed.
func normalizeEvent(frame *wsFrame) (normalizedInbound, bool) {
	body := &frame.Body
	var ev chatKind
	switch body.Event {
	case eventSingleChat, eventGroupChat, eventTeamsPostCreate, eventTeamsPostModify:
	default:
		return normalizedInbound{}, false
	}

	var payload eventPayload
	if len(body.Data) > 0 {
		if err := json.Unmarshal(body.Data, &payload); err != nil {
			return normalizedInbound{}, false
		}
	}

	// Message id: chats carry msgid, teams posts carry post_id. Missing both
	// is a hard drop (see function doc).
	msgID := payload.MsgID
	if msgID == "" {
		msgID = payload.PostID.String()
	}
	if msgID == "" {
		return normalizedInbound{}, false
	}

	msg := channel.InboundMessage{
		EventID:   frame.eventID(),
		MessageID: msgID,
		Type:      channel.MsgTypeText,
		Raw:       marshalRaw(body),
	}
	msg.Source = channel.Source{
		ChannelType: TypeTuitui,
		SenderID:    body.UserAccount,
	}

	switch body.Event {
	case eventSingleChat:
		ev = kindP2P
		chatID := strings.TrimSpace(body.UserAccount)
		if chatID == "" {
			return normalizedInbound{}, false
		}
		msg.Source.ChatID = chatID
		msg.Source.ChatType = channel.ChatTypeP2P
		msg.AddressedToBot = true
		flattenChatMessage(&payload, &msg)

	case eventGroupChat:
		ev = kindGroup
		chatID := payload.GroupID.String()
		if chatID == "" {
			return normalizedInbound{}, false
		}
		msg.Source.ChatID = chatID
		msg.Source.ChatType = channel.ChatTypeGroup
		flattenChatMessage(&payload, &msg)

	default: // teams_post_create / teams_post_modify
		ev = kindTeams
		team := payload.TeamID.String()
		cid := payload.ChannelID.String()
		if team == "" || cid == "" {
			return normalizedInbound{}, false
		}
		thread := payload.ParentID.String()
		if thread == "" || thread == "0" {
			thread = payload.PostID.String()
		}
		chatID := composeTeamsChatID(team, cid, thread)
		msg.Source.ChatID = chatID
		msg.Source.ChatType = channel.ChatTypeGroup
		msg.Source.ThreadID = thread
		// A teams channel subscribed to this bot exists for the bot: the
		// platform carries no mention / is_me signal on posts, and the
		// reference bridge feeds every post to its agent. Treat posts as
		// bot-addressed; the durable (installation, MessageID) dedup still
		// collapses the create/modify pair for one post.
		msg.AddressedToBot = true
		msg.Text = payload.Content
		msg.CommandText = payload.Content
		// Teams media lives in images[]/files[] objects; keep the urls in Raw
		// (the engine MediaResolver owns MediaRefs) and mirror the reference
		// placeholder text so the agent sees that attachments exist.
		for _, img := range payload.Images {
			if img.URL == "" {
				continue
			}
			msg.Text += "\n[图片] " + img.URL
		}
		for _, f := range payload.Files {
			if f.URL == "" {
				continue
			}
			name := f.Name
			if name == "" {
				name = "unknown"
			}
			msg.Text += fmt.Sprintf("\n[文件] %s : %s", name, f.URL)
		}
		if msg.Text == "" {
			// Contentless post with no media: nothing to ingest.
			return normalizedInbound{}, false
		}
	}

	// Reply / addressing semantics follow the reference client: a reply
	// counts as "addressed to this bot" only when ref.is_me proves the
	// quoted message was the bot's, and only then is ReplyTo carried —
	// outbound cannot quote on this platform, so a non-bot quote is just
	// context, appended to the text like the reference client does.
	if payload.Ref != nil && payload.Ref.MsgID != "" {
		if payload.Ref.IsMe {
			msg.ReplyTo = &channel.ReplyCtx{MessageID: payload.Ref.MsgID.String()}
		}
		if strings.TrimSpace(payload.Ref.Content) != "" {
			msg.Text += fmt.Sprintf("\n\n[引用来自 %s 的消息]\n%s", payload.Ref.UserName, payload.Ref.Content)
			msg.HasSelectedContext = true
		}
	}
	if ev == kindGroup {
		msg.AddressedToBot = payload.Ref != nil && payload.Ref.IsMe
	}
	return normalizedInbound{msg: msg, kind: ev}, true
}

// flattenChatMessage maps the plain-chat msg_type union (reference
// _parse_msg_body) onto the normalized text + MsgType. MediaRefs stay empty:
// tuitui hands out directly-fetchable urls that the engine MediaResolver (a
// follow-up wiring) consumes from Raw.
func flattenChatMessage(p *eventPayload, msg *channel.InboundMessage) {
	switch p.MsgType {
	case "text", "":
		msg.Text = p.Text
	case "mixed":
		urls := collectURLs(p.Images)
		t := p.Text
		if len(urls) > 0 && strings.TrimSpace(t) == "" {
			t = strings.Repeat("[图片]\n", len(urls)-1) + "[图片]"
		}
		for _, u := range urls {
			t += "\n[图片] " + u
		}
		msg.Text = t
	case "image":
		msg.Type = channel.MsgTypeImage
		var parts []string
		for _, u := range collectURLs(p.Images) {
			parts = append(parts, "[图片] "+u)
		}
		msg.Text = strings.Join(parts, "\n")
	case "voice":
		msg.Type = channel.MsgTypeAudio
		if p.Voice != "" {
			msg.Text = "[语音] " + p.Voice
		} else {
			msg.Text = "[语音]"
		}
	case "video":
		msg.Type = channel.MsgTypeVideo
		if p.Video != "" {
			msg.Text = "[视频] " + p.Video
		} else {
			msg.Text = "[视频]"
		}
	case "file":
		msg.Type = channel.MsgTypeFile
		name := p.File.Name
		if name == "" {
			name = "unknown"
		}
		if p.File.URL != "" {
			msg.Text = fmt.Sprintf("[文件] %s : %s", name, p.File.URL)
		} else {
			msg.Text = "[文件] " + name
		}
	case "link":
		msg.Text = fmt.Sprintf("[网页链接]\n%s\n%s", p.Link.Title, p.Link.URL)
	default:
		msg.Text = p.Text
	}
	msg.CommandText = msg.Text
}

func collectURLs(parts []flexPart) []string {
	urls := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.URL != "" {
			urls = append(urls, p.URL)
		}
	}
	return urls
}

// teamsChatPrefix marks the adapter's composite teams conversation ids:
// teams_{team_id}_{channel_id}[_{thread_id}]. The engine ChatType enum has
// no "channel" value, so teams rides as ChatTypeGroup with this prefix as
// the platform-side discriminator.
const teamsChatPrefix = "teams_"

func composeTeamsChatID(team, channelID, thread string) string {
	id := teamsChatPrefix + team + "_" + channelID
	if thread != "" {
		id += "_" + thread
	}
	return id
}

// splitTeamsChatID parses a composite teams chat id back into the toteams
// {team_id, channel_id, parent_id} triple. Because the composite is
// underscore-delimited and parsed by position, platform ids must not
// themselves contain underscores: an id splitting into other than 2–3
// non-empty segments (e.g. "teams_a_b_c_d") is malformed and ok=false —
// Send must then fail rather than guess a recipient.
func splitTeamsChatID(chatID string) (team, channelID, thread string, ok bool) {
	rest, found := strings.CutPrefix(chatID, teamsChatPrefix)
	if !found {
		return "", "", "", false
	}
	parts := strings.Split(rest, "_")
	if len(parts) == 2 {
		parts = append(parts, "")
	}
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func marshalRaw(body *eventBody) json.RawMessage {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil
	}
	return raw
}
