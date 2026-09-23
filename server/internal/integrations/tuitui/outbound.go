package tuitui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// This file is the outbound half: it builds the three platform payload
// shapes (plain text to tousers/togroups, teams richtext markdown to
// toteams, interactive cards) and posts them through the shared client.

// sendPayload is the wire shape of POST /message/custom/send and
// /message/custom/modify. One struct covers every shape; omitempty keeps the
// per-kind payload minimal, exactly as the platform expects.
type sendPayload struct {
	MsgType     string          `json:"msgtype"`
	Text        *textContent    `json:"text,omitempty"`
	RichText    *richContent    `json:"richtext,omitempty"`
	Interactive json.RawMessage `json:"interactive,omitempty"`
	Reaction    *reactionBody   `json:"emoji_reaction,omitempty"`
	File        *fileBody       `json:"file,omitempty"`

	// Plain chats: sends target bare id arrays; modify / emoji_reaction
	// target {user|group, msgid} objects.
	ToUsers  []json.RawMessage `json:"tousers,omitempty"`
	ToGroups []json.RawMessage `json:"togroups,omitempty"`

	// Teams conversations take the structured triple instead.
	ToTeams []teamTarget `json:"toteams,omitempty"`
}

type textContent struct {
	Content string `json:"content"`
}

type richContent struct {
	Markdown    string `json:"markdown"`
	DelimsLeft  string `json:"delims_left,omitempty"`
	DelimsRight string `json:"delims_right,omitempty"`
}

type teamTarget struct {
	TeamID    string `json:"team_id"`
	ChannelID string `json:"channel_id"`
	ParentID  string `json:"parent_id"`
	PostID    string `json:"post_id,omitempty"`
}

type reactionBody struct {
	Emoji  string `json:"emoji"`
	Cancel bool   `json:"cancel"`
}

type fileBody struct {
	FID      string `json:"fid"`
	Filename string `json:"filename"`
}

// idTarget renders the bare-string form used by /message/custom/send.
func bareID(id string) json.RawMessage { return mustJSON(id) }

// msgTarget renders the {user|group, msgid} form used by
// /message/custom/modify (card update + emoji reaction).
func userTarget(user, msgID string) json.RawMessage {
	return mustJSON(map[string]string{"user": user, "msgid": msgID})
}

func groupTarget(group, msgID string) json.RawMessage {
	return mustJSON(map[string]string{"group": group, "msgid": msgID})
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("tuitui: payload json: " + err.Error())
	}
	return b
}

// textSendPayload builds a plain-text send for a p2p or group chat.
func textSendPayload(kind chatKind, chatID, content string) (sendPayload, error) {
	p := sendPayload{MsgType: "text", Text: &textContent{Content: content}}
	switch kind {
	case kindP2P:
		p.ToUsers = []json.RawMessage{bareID(chatID)}
	case kindGroup:
		p.ToGroups = []json.RawMessage{bareID(chatID)}
	default:
		return p, fmt.Errorf("tuitui: plain text send needs a p2p or group chat, got kind %d for chat %q", kind, chatID)
	}
	return p, nil
}

// teamsSendPayload builds the markdown send for a teams conversation,
// splitting the composite chat id back into the toteams triple.
func teamsSendPayload(chatID, threadID, markdown string) (sendPayload, error) {
	team, chanID, thread, ok := splitTeamsChatID(chatID)
	if !ok {
		return sendPayload{}, fmt.Errorf("tuitui: malformed teams chat id %q", chatID)
	}
	if thread == "" && threadID != "" {
		thread = threadID
	}
	return sendPayload{
		MsgType:  "richtext/markdown",
		RichText: &richContent{Markdown: markdown},
		ToTeams:  []teamTarget{{TeamID: team, ChannelID: chanID, ParentID: thread}},
	}, nil
}

// splitChunks splits one outbound body at maxLen, preferring paragraph and
// line boundaries (reference _split_message): break at the last "\n\n"
// before the cap, else the last "\n", else hard-cut.
func splitChunks(text string, maxLen int) []string {
	runes := []rune(text)
	if maxLen <= 0 || len(runes) <= maxLen {
		return []string{text}
	}
	var chunks []string
	for len(runes) > maxLen {
		cut := -1
		window := runes[:maxLen]
		for i := len(window) - 1; i >= 1; i-- {
			if window[i] == '\n' && window[i-1] == '\n' {
				cut = i + 1
				break
			}
		}
		if cut < 0 {
			for i := len(window) - 1; i >= 0; i-- {
				if window[i] == '\n' {
					cut = i + 1
					break
				}
			}
		}
		if cut < 0 {
			cut = maxLen
		}
		chunks = append(chunks, string(runes[:cut]))
		runes = runes[cut:]
		for len(runes) > 0 && runes[0] == '\n' {
			runes = runes[1:]
		}
	}
	if len(runes) > 0 {
		chunks = append(chunks, string(runes))
	}
	return chunks
}

// interactiveSend builds an interactive-card send for a p2p/group chat.
// Teams conversations are rejected: the platform does not render cards
// there (the reference client warns and skips).
func interactiveSend(kind chatKind, chatID string, card json.RawMessage) (sendPayload, error) {
	if kind == kindTeams {
		return sendPayload{}, errors.New("tuitui: interactive cards are not supported in teams conversations")
	}
	p := sendPayload{MsgType: "interactive", Interactive: card}
	switch kind {
	case kindP2P:
		p.ToUsers = []json.RawMessage{bareID(chatID)}
	case kindGroup:
		p.ToGroups = []json.RawMessage{bareID(chatID)}
	default:
		return sendPayload{}, fmt.Errorf("tuitui: interactive send needs a known p2p or group chat, got chat %q", chatID)
	}
	return p, nil
}

// cardModify builds the /message/custom/modify payload that updates an
// existing interactive card in place — the primitive a later streaming
// renderer edits its reply with.
func cardModify(kind chatKind, chatID, msgID string, card json.RawMessage) (sendPayload, error) {
	if kind == kindTeams {
		return sendPayload{}, errors.New("tuitui: interactive cards are not supported in teams conversations")
	}
	p := sendPayload{MsgType: "interactive", Interactive: card}
	switch kind {
	case kindP2P:
		p.ToUsers = []json.RawMessage{userTarget(chatID, msgID)}
	case kindGroup:
		p.ToGroups = []json.RawMessage{groupTarget(chatID, msgID)}
	default:
		return sendPayload{}, fmt.Errorf("tuitui: card modify needs a known p2p or group chat, got chat %q", chatID)
	}
	return p, nil
}

// reactionBody builds an emoji_reaction modify for any conversation kind.
func reactionPayload(kind chatKind, chatID, msgID, emoji string) (sendPayload, error) {
	p := sendPayload{MsgType: "emoji_reaction", Reaction: &reactionBody{Emoji: emoji, Cancel: false}}
	switch kind {
	case kindP2P:
		p.ToUsers = []json.RawMessage{userTarget(chatID, msgID)}
	case kindGroup:
		p.ToGroups = []json.RawMessage{groupTarget(chatID, msgID)}
	case kindTeams:
		team, chanID, thread, ok := splitTeamsChatID(chatID)
		if !ok {
			return sendPayload{}, fmt.Errorf("tuitui: malformed teams chat id %q", chatID)
		}
		p.ToTeams = []teamTarget{{TeamID: team, ChannelID: chanID, ParentID: thread, PostID: msgID}}
	default:
		return sendPayload{}, fmt.Errorf("tuitui: reaction needs a known conversation kind for chat %q", chatID)
	}
	return p, nil
}

// fileSend builds the non-teams file send (uploaded fid + display name);
// teams get a markdown link embedding the fid through the platform's
// {{tuitui_file "fid"}} directive.
func fileSend(kind chatKind, chatID, fid, filename string) (sendPayload, error) {
	switch kind {
	case kindP2P:
		return sendPayload{
			MsgType: "file",
			File:    &fileBody{FID: fid, Filename: filename},
			ToUsers: []json.RawMessage{bareID(chatID)},
		}, nil
	case kindGroup:
		return sendPayload{
			MsgType:  "file",
			File:     &fileBody{FID: fid, Filename: filename},
			ToGroups: []json.RawMessage{bareID(chatID)},
		}, nil
	case kindTeams:
		p, err := teamsSendPayload(chatID, "", fmt.Sprintf("[%s]({{tuitui_file %q}})", filename, fid))
		if err != nil {
			return p, err
		}
		p.RichText.DelimsLeft = "{{"
		p.RichText.DelimsRight = "}}"
		return p, nil
	default:
		return sendPayload{}, fmt.Errorf("tuitui: file send needs a known conversation kind for chat %q", chatID)
	}
}

// Send delivers the outbound reply. Chunk ids are collected into
// SendResult.MessageIDs so a later card renderer can address any part.
func (c *tuituiChannel) Send(ctx context.Context, out channel.OutboundMessage) (channel.SendResult, error) {
	if strings.TrimSpace(out.Text) == "" {
		return channel.SendResult{}, nil
	}
	if strings.TrimSpace(out.ChatID) == "" {
		return channel.SendResult{}, errors.New("tuitui: Send needs a non-empty ChatID")
	}
	chunks := splitChunks(out.Text, c.cfg.MaxMessageLength)
	var res channel.SendResult
	for _, chunk := range chunks {
		payload, err := c.sendPayloadFor(out.ChatID, out.ThreadID, chunk)
		if err != nil {
			return res, err
		}
		id, err := c.post(ctx, pathSend, payload)
		if id != "" {
			res.MessageID = id
			res.MessageIDs = append(res.MessageIDs, id)
		}
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

// sendPayloadFor routes one chunk. Teams is decided exclusively by the
// "teams_" prefix on the ChatID (never by the in-memory kind map) so the
// routing survives process restarts: the composite id is persisted by the
// core in channel_chat_session_binding.channel_chat_id. Teams always degrades
// to the markdown shape — the platform renders no interactive cards there.
// Plain chats pick tousers vs togroups from the kind the receive loop
// recorded from the real event type, with a platform ID-format backstop
// after a restart (see routeKind).
func (c *tuituiChannel) sendPayloadFor(chatID, threadID, content string) (sendPayload, error) {
	kind := c.routeKind(chatID)
	if kind == kindTeams {
		return teamsSendPayload(chatID, threadID, content)
	}
	return textSendPayload(kind, chatID, content)
}

// routeKind classifies a ChatID for outbound payload selection. Teams is
// prefix-only and map-independent. For plain chats the recorded event kind
// wins; when the map is cold (process restart) the platform's own ID format
// is the backstop: group ids are pure numeric, user accounts are not.
func (c *tuituiChannel) routeKind(chatID string) chatKind {
	if strings.HasPrefix(chatID, teamsChatPrefix) {
		return kindTeams
	}
	if k := c.kindOf(chatID); k == kindP2P || k == kindGroup {
		return k
	}
	if isNumericChatID(chatID) {
		return kindGroup
	}
	return kindP2P
}

// isNumericChatID reports a non-empty all-digits chat id.
func isNumericChatID(chatID string) bool {
	if chatID == "" {
		return false
	}
	for _, r := range chatID {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// post sends one payload and extracts the delivered message id. A non-nil
// error with a non-empty id still means that chunk may have been delivered.
func (c *tuituiChannel) post(ctx context.Context, path string, payload sendPayload) (string, error) {
	resp, err := c.client.postJSON(ctx, path, payload)
	if err != nil {
		var id string
		if resp != nil {
			id = resp.messageID()
		}
		return id, fmt.Errorf("tuitui: %s: %w", path, err)
	}
	return resp.messageID(), nil
}

// SendInteractive delivers a rich card into a p2p/group chat and returns the
// platform msgid. Teams chats always error (no card support). Exposed for
// the follow-up streaming-card renderer, not the generic Send envelope.
func (c *tuituiChannel) SendInteractive(ctx context.Context, chatID string, card json.RawMessage) (string, error) {
	payload, err := interactiveSend(c.routeKind(chatID), chatID, card)
	if err != nil {
		return "", err
	}
	return c.post(ctx, pathSend, payload)
}

// UpdateInteractive replaces an interactive card in place via
// /message/custom/modify. Exposed for the follow-up streaming renderer.
func (c *tuituiChannel) UpdateInteractive(ctx context.Context, chatID, msgID string, card json.RawMessage) error {
	payload, err := cardModify(c.routeKind(chatID), chatID, msgID, card)
	if err != nil {
		return err
	}
	_, err = c.post(ctx, pathModify, payload)
	return err
}

// SendReaction adds the platform emoji/text reaction to a message (the
// reference bridge acks intake with "收到"). Best-effort: callers log errors.
func (c *tuituiChannel) SendReaction(ctx context.Context, chatID, msgID, emoji string) error {
	if emoji == "" {
		emoji = c.cfg.ReactionEmoji
	}
	payload, err := reactionPayload(c.routeKind(chatID), chatID, msgID, emoji)
	if err != nil {
		return err
	}
	_, err = c.post(ctx, pathModify, payload)
	return err
}

// UploadMedia posts a multipart upload and returns the platform fid. Exposed
// for the follow-up outbound attachment path.
func (c *tuituiChannel) UploadMedia(ctx context.Context, mediaType, filename string, content []byte) (string, error) {
	return c.client.uploadMedia(ctx, mediaType, filename, content)
}

// SendFile uploads one file and sends it as a native file message (or, for
// teams, a markdown {{tuitui_file}} link). Exposed for the follow-up
// outbound attachment path.
func (c *tuituiChannel) SendFile(ctx context.Context, chatID, mediaType, filename string, content []byte) (string, error) {
	fid, err := c.client.uploadMedia(ctx, mediaType, filename, content)
	if err != nil {
		return "", err
	}
	payload, err := fileSend(c.routeKind(chatID), chatID, fid, filename)
	if err != nil {
		return "", err
	}
	return c.post(ctx, pathSend, payload)
}
