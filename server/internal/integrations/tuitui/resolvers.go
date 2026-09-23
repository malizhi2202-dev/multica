package tuitui

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// This file is the Tuitui ResolverSet: the platform-specific seams the
// channel-agnostic engine.Router runs the inbound pipeline through. It is
// built entirely on the generic channel_* queries plus the shared
// engine.ChatSession, mirroring the DingTalk / Slack / WeCom ResolverSets.
//
// OriginType stays empty on purpose: the live issue.origin_type CHECK lists
// the channel origins added so far but not 'tuitui_chat', and this change
// ships no migration. A /issue typed in a Tuitui chat therefore creates the
// issue with a NULL origin (IssueService skips the origin columns when
// OriginType is ""), which is honest rather than a mislabeled origin.
// Stamping it is a one-line change plus the standard widen-CHECK migration
// pair (the 366/367 pattern).

// NewTuituiResolverSet assembles the Tuitui ResolverSet over the generated
// queries + a tx starter (for the shared session service). The replier
// delivers the outbound binding-prompt / status / issue-created notices;
// pass a nil engine.OutboundReplier to disable them. Typing is not wired:
// the platform has emoji reactions but no per-session indicator lifecycle to
// clear on a run that produced no task, so the slot stays nil (the Router
// treats nil as "disabled", never as a typed-nil). Media is likewise nil:
// inbound keeps media urls in Text/Raw and emits no MediaRefs, so there is
// nothing for a MediaResolver to resolve yet.
func NewTuituiResolverSet(q *db.Queries, tx engine.TxStarter, replier engine.OutboundReplier) engine.ResolverSet {
	return engine.ResolverSet{
		Installation: &installationResolver{q: q},
		Identity:     &identityResolver{q: q},
		Dedup:        &deduper{q: q},
		Session: &sessionBinder{session: engine.NewChatSession(q, tx, TypeTuitui, engine.SessionTitles{
			Group:    "Tuitui group",
			Direct:   "Tuitui direct message",
			Fallback: "Tuitui chat",
		})},
		Audit:      &auditor{q: q},
		Replier:    replier,
		OriginType: "",
	}
}

var (
	_ engine.InstallationResolver = (*installationResolver)(nil)
	_ engine.IdentityResolver     = (*identityResolver)(nil)
	_ engine.Deduper              = (*deduper)(nil)
	_ engine.SessionBinder        = (*sessionBinder)(nil)
	_ engine.Auditor              = (*auditor)(nil)
)

// tuituiSessionRouting derives the session-isolation key from one inbound
// message. Tuitui conversation ids are already session-scoped by the
// platform: a p2p ChatID is the sender's user account, a group ChatID is
// the group id, and a teams ChatID is the composite
// "teams_{team}_{channel}[_{thread}]" the receive loop composes (threads
// isolated like Slack). The key alone therefore carries everything the
// outbound path needs — chat_type plus the prefix identify the payload
// shape — so the binding config stays empty.
func tuituiSessionRouting(msg channel.InboundMessage) (bindingKey string, config []byte) {
	return msg.Source.ChatID, nil
}

// kindForSource maps the durable (chat type, chat id) pair back onto the
// adapter's outbound payload kind. Teams is decided exclusively by the
// "teams_" prefix (it rides ChatTypeGroup because the engine ChatType enum
// is closed), p2p by ChatTypeP2P, everything else by the plain-group shape.
// It is the restart-safe inverse of normalizeEvent's routing: no in-memory
// kind map participates, so a reply can never be sent with the wrong shape
// after a process restart.
func kindForSource(chatType, chatID string) chatKind {
	if strings.HasPrefix(chatID, teamsChatPrefix) {
		return kindTeams
	}
	if chatType == string(channel.ChatTypeP2P) {
		return kindP2P
	}
	return kindGroup
}

func kindForMessage(msg channel.InboundMessage) chatKind {
	return kindForSource(string(msg.Source.ChatType), msg.Source.ChatID)
}

func nullText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// ---- installation routing ----

type installationResolver struct{ q *db.Queries }

func (r *installationResolver) ResolveInstallation(ctx context.Context, msg channel.InboundMessage) (engine.ResolvedInstallation, error) {
	raw, err := decodeTuituiRaw(msg)
	if err != nil {
		return engine.ResolvedInstallation{}, err
	}
	// Route by the app_id the receiving connection stamped into the envelope
	// (one WebSocket per installation authenticates with exactly one
	// app_id/app_secret pair, so the stamp uniquely identifies the
	// installation — the platform frame itself carries no bot identity).
	if raw.AppID == "" {
		return engine.ResolvedInstallation{}, engine.ErrInstallationNotFound
	}
	inst, err := r.q.GetChannelInstallationByAppID(ctx, db.GetChannelInstallationByAppIDParams{
		ChannelType: string(TypeTuitui),
		AppID:       raw.AppID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return engine.ResolvedInstallation{}, engine.ErrInstallationNotFound
		}
		return engine.ResolvedInstallation{}, err
	}
	return engine.ResolvedInstallation{
		ID:              inst.ID,
		WorkspaceID:     inst.WorkspaceID,
		AgentID:         inst.AgentID,
		InstallerUserID: inst.InstallerUserID,
		Active:          inst.Status == "active",
		Platform:        inst,
	}, nil
}

// ---- identity ----

type identityResolver struct{ q *db.Queries }

func (r *identityResolver) ResolveSender(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage) (engine.ResolvedIdentity, error) {
	senderID := strings.TrimSpace(msg.Source.SenderID)
	if senderID == "" {
		return engine.ResolvedIdentity{}, engine.ErrSenderUnbound
	}
	binding, err := r.q.GetChannelUserBindingByUserID(ctx, db.GetChannelUserBindingByUserIDParams{
		InstallationID: inst.ID,
		ChannelUserID:  senderID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return engine.ResolvedIdentity{}, engine.ErrSenderUnbound
		}
		return engine.ResolvedIdentity{}, err
	}
	// Binding existence no longer proves membership (no FK); re-check.
	if _, err := r.q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID:      binding.MulticaUserID,
		WorkspaceID: inst.WorkspaceID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return engine.ResolvedIdentity{}, engine.ErrSenderNotMember
		}
		return engine.ResolvedIdentity{}, err
	}
	return engine.ResolvedIdentity{UserID: binding.MulticaUserID}, nil
}

// ---- dedup ----

type deduper struct{ q *db.Queries }

func (r *deduper) Claim(ctx context.Context, installationID pgtype.UUID, messageID string) (pgtype.UUID, error) {
	claim, err := r.q.ClaimChannelInboundDedup(ctx, db.ClaimChannelInboundDedupParams{
		InstallationID: installationID,
		MessageID:      messageID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, engine.ErrDuplicate
		}
		return pgtype.UUID{}, err
	}
	return claim.ClaimToken, nil
}

func (r *deduper) Mark(ctx context.Context, installationID pgtype.UUID, messageID string, claimToken pgtype.UUID) error {
	_, err := r.q.MarkChannelInboundDedupProcessed(ctx, db.MarkChannelInboundDedupProcessedParams{
		InstallationID: installationID,
		MessageID:      messageID,
		ClaimToken:     claimToken,
	})
	return err
}

func (r *deduper) Release(ctx context.Context, installationID pgtype.UUID, messageID string, claimToken pgtype.UUID) error {
	_, err := r.q.ReleaseChannelInboundDedup(ctx, db.ReleaseChannelInboundDedupParams{
		InstallationID: installationID,
		MessageID:      messageID,
		ClaimToken:     claimToken,
	})
	return err
}

// ---- session bind / append ----

type chatSession interface {
	EnsureSession(ctx context.Context, in engine.EnsureSessionInput) (pgtype.UUID, error)
	StartSession(ctx context.Context, in engine.StartSessionInput) (engine.StartSessionResult, error)
	MarkPendingFresh(ctx context.Context, sessionID pgtype.UUID, messageID string) error
	AppendUserMessage(ctx context.Context, in engine.AppendInput) (engine.AppendResult, error)
	BindMediaRefs(ctx context.Context, in engine.BindMediaInput) error
}

type sessionBinder struct {
	session chatSession
}

func (r *sessionBinder) EnsureSession(ctx context.Context, p engine.EnsureSessionParams) (pgtype.UUID, error) {
	bindingKey, config := tuituiSessionRouting(p.Message)
	return r.session.EnsureSession(ctx, engine.EnsureSessionInput{
		WorkspaceID:    p.Installation.WorkspaceID,
		AgentID:        p.Installation.AgentID,
		InstallationID: p.Installation.ID,
		Sender:         p.Sender,
		BindingKey:     bindingKey,
		BindingConfig:  config,
		ChatType:       p.Message.Source.ChatType,
	})
}

func (r *sessionBinder) StartSession(ctx context.Context, p engine.StartSessionParams) (engine.StartSessionResult, error) {
	bindingKey, config := tuituiSessionRouting(p.Message)
	return r.session.StartSession(ctx, engine.StartSessionInput{
		EnsureSessionInput: engine.EnsureSessionInput{
			WorkspaceID: p.Installation.WorkspaceID, AgentID: p.Installation.AgentID,
			InstallationID: p.Installation.ID, Sender: p.Creator,
			BindingKey: bindingKey, BindingConfig: config, ChatType: p.Message.Source.ChatType,
		},
		Initiator: p.Sender,
		Body:      p.Message.Text, CommandText: p.Message.CommandText, MessageID: p.Message.MessageID, ThreadID: p.Message.Source.ThreadID,
		ClaimToken: p.ClaimToken, MediaPendingSeconds: p.MediaPendingSeconds,
		PersistMessage: p.PersistMessage, HistoryBoundaryPending: p.HistoryBoundaryPending,
		BeforeCommit: p.BeforeCommit,
	})
}

func (r *sessionBinder) MarkPendingFresh(ctx context.Context, sessionID pgtype.UUID, messageID string) error {
	return r.session.MarkPendingFresh(ctx, sessionID, messageID)
}

func (r *sessionBinder) AppendMessage(ctx context.Context, p engine.AppendParams) (engine.AppendResult, error) {
	commandText := p.Message.CommandText
	if commandText == "" {
		commandText = p.Message.Text
	}
	return r.session.AppendUserMessage(ctx, engine.AppendInput{
		SessionID:           p.SessionID,
		Sender:              p.Sender,
		InstallationID:      p.InstallationID,
		Body:                p.Message.Text,
		CommandText:         commandText,
		MessageID:           p.Message.MessageID,
		ThreadID:            p.Message.Source.ThreadID,
		ClaimToken:          p.ClaimToken,
		MediaPendingSeconds: p.MediaPendingSeconds,
		ForceFresh:          p.Message.ForceFresh,
	})
}

func (r *sessionBinder) BindMedia(ctx context.Context, p engine.BindMediaParams) (engine.BindMediaResult, error) {
	in := engine.BindMediaInput{
		MessageID:            p.MessageID,
		SessionID:            p.SessionID,
		WorkspaceID:          p.WorkspaceID,
		Sender:               p.Sender,
		IssueID:              p.IssueID,
		IssueDescriptionBase: p.IssueDescriptionBase,
		IssueCommandText:     p.IssueCommandText,
		Body:                 p.Body,
		MediaRefs:            p.MediaRefs,
	}
	if richer, ok := r.session.(interface {
		BindMediaRefsWithResult(context.Context, engine.BindMediaInput) (engine.BindMediaResult, error)
	}); ok {
		return richer.BindMediaRefsWithResult(ctx, in)
	}
	return engine.BindMediaResult{}, r.session.BindMediaRefs(ctx, in)
}

// ---- audit ----

type auditor struct{ q *db.Queries }

func (r *auditor) RecordDrop(ctx context.Context, instID pgtype.UUID, msg channel.InboundMessage, reason engine.DropReason) error {
	return r.q.RecordChannelInboundDrop(ctx, db.RecordChannelInboundDropParams{
		ID:               dbid.NewV7(),
		ChannelType:      string(TypeTuitui),
		EventType:        dropEventType(msg),
		DropReason:       string(reason),
		InstallationID:   instID,
		ChannelChatID:    nullText(msg.Source.ChatID),
		ChannelEventID:   nullText(msg.EventID),
		ChannelMessageID: nullText(msg.MessageID),
	})
}

// dropEventType names the platform event behind a dropped frame
// (single_chat / group_chat / teams_post_create / …) so the drop audit can
// tell the three conversation shapes apart. Unparsable Raw degrades to the
// generic "message" label rather than losing the audit row.
func dropEventType(msg channel.InboundMessage) string {
	if raw, err := decodeTuituiRaw(msg); err == nil && raw.Body.Event != "" {
		return raw.Body.Event
	}
	return "message"
}
