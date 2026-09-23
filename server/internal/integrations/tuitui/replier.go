package tuitui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// This file is the Tuitui engine.OutboundReplier — the seam that delivers a
// verdict-driven reply back to the user. It posts through the same robot API
// as the chat-done subscriber (outbound_chat.go), resolving the per-
// installation credentials from the installation row the Router carried.
//
// Outcomes handled (mirroring the DingTalk replier's wording structure):
//   - NeedsBinding: the sender is unbound. Mint a single-use binding token
//     and reply with a "link your account" prompt pointing at the in-product
//     redeem page (/tuitui/bind), delivered PRIVATELY to the sender's user
//     account. After they bind, their next message reaches the agent.
//   - AgentOffline / AgentArchived: a status notice so the user is not left
//     wondering why nothing happened.
//   - FreshPending / ChatStarted / IssueUsage: command confirmations.
//   - Ingested with a synchronously-created /issue: a confirmation carrying
//     the issue identifier and title. Plain chat turns stay silent — the
//     agent's own reply is delivered by the chat-done subscriber.

const (
	agentOfflineText        = "⚠️ The agent is offline, so this message won't be processed automatically."
	agentArchivedText       = "⚠️ This agent has been archived and can't respond. Please contact your workspace admin."
	freshPendingText        = "✅ Fresh start ready. Your next chat message will run without previous context."
	chatStartedText         = "✅ Started a new Multica chat. Your next message will enter it."
	issueUsageText          = "Please include an issue title. Use:\n\n`/issue <title>`\n\n`[description]` (optional)"
	issueUsageWithMediaText = "Please add a title and resend with the image (*image can come before or after the command*):\n\n`/issue <title>`\n\n`[description]` (optional)"
	// Refusals for dropped /issue commands: without them the user's command
	// vanishes with no signal that it will never be handled.
	issueNotMemberText = "You're not a member of this Multica workspace, so I can't file an issue for you. Ask a workspace admin to invite you, then send the command again."
	issueDisabledText  = "This Tuitui robot isn't connected to Multica (or was disconnected). Ask the agent owner or a workspace owner/admin to reconnect it."
)

// bindingMinter is the binding-token surface the replier needs.
// *BindingTokenService satisfies it.
type bindingMinter interface {
	Mint(ctx context.Context, workspaceID, installationID pgtype.UUID, tuituiUserID string) (BindingToken, error)
}

// OutboundReplier implements engine.OutboundReplier for Tuitui.
type OutboundReplier struct {
	binding     bindingMinter
	decrypt     Decrypter
	httpClient  *http.Client
	appURL      string
	bindingPath string
	logger      *slog.Logger
}

// OutboundReplierConfig configures the replier. Binding + AppURL are required
// for the NeedsBinding prompt to work; without them the prompt is skipped
// (the status and command notices still fire).
type OutboundReplierConfig struct {
	Binding bindingMinter
	Decrypt Decrypter
	// HTTPClient overrides the outbound send client (tests inject an
	// httptest client). Nil uses a default with a request-scoped timeout.
	HTTPClient *http.Client
	// AppURL is the Multica web app host the user clicks into to redeem the
	// binding token (e.g. https://multica.example). The bind page
	// (/tuitui/bind) is served by the web app, so the link must point at the
	// app host, not the API host. Mirrors the DingTalk/Slack repliers.
	AppURL      string
	BindingPath string // default "/tuitui/bind"
	Logger      *slog.Logger
}

var _ engine.OutboundReplier = (*OutboundReplier)(nil)

// NewOutboundReplier builds the replier.
func NewOutboundReplier(cfg OutboundReplierConfig) *OutboundReplier {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	bindingPath := cfg.BindingPath
	if bindingPath == "" {
		bindingPath = "/tuitui/bind"
	}
	if !strings.HasPrefix(bindingPath, "/") {
		bindingPath = "/" + bindingPath
	}
	return &OutboundReplier{
		binding:     cfg.Binding,
		decrypt:     cfg.Decrypt,
		httpClient:  cfg.HTTPClient,
		appURL:      strings.TrimRight(cfg.AppURL, "/"),
		bindingPath: bindingPath,
		logger:      logger,
	}
}

// Reply routes each outcome to its user-visible message. Errors are logged,
// not propagated: the replier runs detached from the inbound ACK path.
func (r *OutboundReplier) Reply(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, res engine.Result) {
	switch res.Outcome {
	case engine.OutcomeNeedsBinding:
		if err := r.sendBindingPrompt(ctx, inst, msg, res); err != nil {
			r.logger.WarnContext(ctx, "tuitui replier: binding prompt failed",
				"installation_id", util.UUIDToString(inst.ID), "error", err)
		}
	case engine.OutcomeAgentOffline:
		r.post(ctx, inst, msg, agentOfflineText, "offline notice")
	case engine.OutcomeAgentArchived:
		r.post(ctx, inst, msg, agentArchivedText, "archived notice")
	case engine.OutcomeFreshPending:
		r.post(ctx, inst, msg, freshPendingText, "fresh-start confirmation")
	case engine.OutcomeChatStarted:
		r.post(ctx, inst, msg, chatStartedText, "new-chat confirmation")
	case engine.OutcomeIssueUsage:
		text := issueUsageText
		if res.IssueUsageHadMedia {
			text = issueUsageWithMediaText
		}
		r.post(ctx, inst, msg, text, "issue usage reply")
	case engine.OutcomeIngested:
		if res.IssueID.Valid {
			text := issueCreatedText(res, r.appURL)
			if res.IssueDuplicate {
				text = issueDuplicateText(res, r.appURL)
			}
			r.post(ctx, inst, msg, text, "issue outcome reply")
		}
	case engine.OutcomeDropped:
		// Dropped /issue commands get a refusal so the sender is not left
		// waiting for an issue that will never be created; every other drop
		// (duplicates, unaddressed group chatter) stays silent.
		if text := droppedReplyText(res, msg); text != "" {
			r.post(ctx, inst, msg, text, "drop refusal")
		}
	}
}

func (r *OutboundReplier) post(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, text, what string) {
	if _, err := sendInstallationText(ctx, r.decrypt, r.httpClient, inst, targetFromMessage(msg), text); err != nil {
		r.logger.WarnContext(ctx, "tuitui replier: "+what+" failed",
			"installation_id", util.UUIDToString(inst.ID), "error", err)
	}
}

// sendBindingPrompt mints the single-use token and delivers the redeem link.
func (r *OutboundReplier) sendBindingPrompt(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, res engine.Result) error {
	sender := res.Sender
	if sender == "" {
		sender = msg.Source.SenderID
	}
	sender = strings.TrimSpace(sender)
	if sender == "" {
		return errors.New("missing sender id")
	}
	if r.binding == nil {
		return errors.New("binding service not configured")
	}
	if r.appURL == "" {
		return errors.New("app url not configured")
	}
	token, err := r.binding.Mint(ctx, inst.WorkspaceID, inst.ID, sender)
	if err != nil {
		return fmt.Errorf("mint binding token: %w", err)
	}
	bindURL := r.appURL + r.bindingPath + "?token=" + url.QueryEscape(token.Raw)
	text := "👋 To start chatting with me, link your Tuitui account to Multica: [link your account](" +
		bindURL + ")\n\n(This link expires in 15 minutes.)"
	// Deliver the single-use binding link privately (tousers) to the sender,
	// never into the conversation: in a group or teams channel that would
	// post the token where any other member could redeem it and bind the
	// sender's Tuitui account to their own Multica user (identity
	// misbinding). A direct tousers send works from any conversation kind
	// because every Tuitui event carries the sender's user account.
	if _, err := sendInstallationText(ctx, r.decrypt, r.httpClient, inst, sendTarget{kind: kindP2P, chatID: sender}, text); err != nil {
		return fmt.Errorf("post tuitui binding prompt: %w", err)
	}
	return nil
}

// targetFromMessage builds the reply target from the inbound message's own
// routing identity (used for the immediate status/command replies, before —
// or without — any chat binding existing).
func targetFromMessage(msg channel.InboundMessage) sendTarget {
	return sendTarget{
		kind:     kindForMessage(msg),
		chatID:   msg.Source.ChatID,
		threadID: msg.Source.ThreadID,
	}
}

// sendTarget addresses one Tuitui conversation for a proactive send. kind
// picks the payload shape (tousers / togroups / toteams); chatID is the
// platform id (or composite teams id) the shape consumes; threadID only
// matters for a teams composite without an embedded thread.
type sendTarget struct {
	kind     chatKind
	chatID   string
	threadID string
}

// sendInstallationText resolves an installation's credentials from the carried
// platform row and sends text into target. The effective host/port come from
// the stored config (FromInstallConfig applies the adapter defaults only for
// keys the row omits) — never hardcoded here. Returns the last delivered
// platform msgid.
func sendInstallationText(ctx context.Context, decrypt Decrypter, httpClient *http.Client, inst engine.ResolvedInstallation, target sendTarget, text string) (string, error) {
	row, ok := inst.Platform.(db.ChannelInstallation)
	if !ok {
		return "", errors.New("tuitui: installation platform row unavailable")
	}
	cfg, err := FromConfigDecrypted(channelConfig{Type: TypeTuitui, Raw: row.Config}, decrypt)
	if err != nil {
		return "", fmt.Errorf("decode tuitui credentials: %w", err)
	}
	c := newClient(cfg, httpClient)
	var lastID string
	for _, chunk := range splitChunks(text, cfg.MaxMessageLength) {
		payload, err := c.sendPayloadFor(target, chunk)
		if err != nil {
			return lastID, err
		}
		resp, err := c.postJSON(ctx, pathSend, payload)
		if resp != nil {
			if id := resp.messageID(); id != "" {
				lastID = id
			}
		}
		if err != nil {
			return lastID, fmt.Errorf("tuitui send: %w", err)
		}
	}
	return lastID, nil
}

// sendPayloadFor routes one chunk through the three platform payload shapes.
// Shared by the replier and the chat-done subscriber; it is the stateless
// counterpart of (*tuituiChannel).sendPayloadFor, which may additionally use
// the connection's in-memory kind map.
func (c *client) sendPayloadFor(target sendTarget, content string) (sendPayload, error) {
	if target.kind == kindTeams {
		return teamsSendPayload(target.chatID, target.threadID, content)
	}
	return textSendPayload(target.kind, target.chatID, content)
}

// isAddressedIssueCommand reports whether msg is an /issue command explicitly
// addressed to the bot — only such messages warrant an error/refusal reply:
// the sender asked for an action, so silence would read as acceptance.
func isAddressedIssueCommand(msg channel.InboundMessage) bool {
	if !msg.AddressedToBot {
		return false
	}
	source := msg.CommandText
	if source == "" {
		source = msg.Text
	}
	_, ok := engine.ParseIssueCommand(source)
	return ok
}

// droppedReplyText maps an OutcomeDropped result to a user-facing refusal.
func droppedReplyText(res engine.Result, msg channel.InboundMessage) string {
	if !isAddressedIssueCommand(msg) {
		return ""
	}
	switch res.DropReason {
	case engine.DropReasonNonWorkspaceMember:
		return issueNotMemberText
	case engine.DropReasonRevokedInstallation:
		return issueDisabledText
	default:
		return ""
	}
}

func issueCreatedText(res engine.Result, appURL string) string {
	identifier := issueMarkdownIdentifier(res, appURL)
	if res.IssueTitle == "" {
		return "✅ Created " + identifier
	}
	return "✅ Created " + identifier + " — " + res.IssueTitle
}

func issueDuplicateText(res engine.Result, appURL string) string {
	identifier := issueMarkdownIdentifier(res, appURL)
	if res.IssueTitle == "" {
		return "⚠️ Not created — active issue " + identifier + " already exists."
	}
	return "⚠️ Not created — active issue " + identifier + " already exists: " + res.IssueTitle
}

// escapeMarkdownText neutralizes brackets and backticks in a displayed
// identifier so it cannot break the surrounding Markdown link.
func escapeMarkdownText(s string) string {
	return strings.NewReplacer("[", "\\[", "]", "\\]", "`", "\\`").Replace(s)
}

// Link the displayed issue key to its stable UUID within the installation's
// workspace, the way the DingTalk replier does: legacy key-based URLs
// depend on the reader's last workspace, and a bare #number is not a
// routable issue identifier.
func issueMarkdownIdentifier(res engine.Result, appURL string) string {
	identifier := issueResultIdentifier(res)
	workspaceSlug := strings.TrimSpace(res.IssueWorkspaceSlug)
	if !res.IssueID.Valid || workspaceSlug == "" {
		return identifier
	}
	base, err := url.Parse(strings.TrimSpace(appURL))
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return identifier
	}
	href := channel.IssueWebLink(base.String(), workspaceSlug, util.UUIDToString(res.IssueID))
	// Parentheses in an application's base path must not terminate the Markdown
	// link destination, even when they are valid URL path characters.
	href = strings.NewReplacer("(", "%28", ")", "%29").Replace(href)
	return "[" + escapeMarkdownText(identifier) + "](" + href + ")"
}

func issueResultIdentifier(res engine.Result) string {
	if res.IssueIdentifier != "" {
		return res.IssueIdentifier
	}
	if res.IssueNumber > 0 {
		return fmt.Sprintf("#%d", res.IssueNumber)
	}
	return util.UUIDToString(res.IssueID)
}
