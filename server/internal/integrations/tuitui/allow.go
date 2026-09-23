package tuitui

import (
	"context"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
)

// This file is the Tuitui inbound allowlist: who may talk to a bot at all.
// The rule is the reference bridge client's `_is_allowed()` — a DM is checked
// against allow_from, and a group/channel message is checked against
// group_allow_from first, falling back to allow_from for the SENDER when the
// conversation is not listed. Two deliberate deviations from that
// implementation are recorded where they apply:
//
//  1. An UNCONFIGURED list (no key in the stored installation config) imposes
//     no restriction instead of denying everyone. Every installation created
//     before this feature stores no key, so copying the reference's "empty =
//     deny" would silently switch already-working bots back to dropping every
//     message the moment this build starts — with no reply and no error the
//     operator can see. An explicit empty list still fails closed, and the two
//     shapes stay distinguishable in storage (see installConfig).
//  2. A refused sender is audited (channel_inbound_audit) rather than only
//     logged, because these ids are opaque platform strings nobody can guess:
//     the Settings panel reads those rows back and offers them as one-click
//     entries.

// AllowlistReason is the drop_reason recorded for a message this gate refused.
// It is a plain data label in the engine's shared audit vocabulary, and the
// read-only rejections endpoint selects on it.
const AllowlistReason = "not_allowlisted"

// allowsDirect reports whether one DM may reach the core. userAccount is the
// platform sender account, which is also the p2p chat id on this platform.
func (c Config) allowsDirect(userAccount string) bool {
	return c.AllowFrom.Allows(strings.TrimSpace(userAccount))
}

// allowsConversation reports whether a group or teams message may reach the
// core: first the conversation against GroupAllowFrom, then — per the
// reference's fallback — the sender against AllowFrom. Both checks treat an
// unconfigured list as unrestricted, so an installation with neither key keeps
// answering exactly as it did before this gate existed.
func (c Config) allowsConversation(kind chatKind, chatID, userAccount string) bool {
	for _, candidate := range conversationCandidates(kind, chatID) {
		if c.GroupAllowFrom.Allows(candidate) {
			return true
		}
	}
	return c.AllowFrom.Allows(strings.TrimSpace(userAccount))
}

// allowsInbound applies the gate to one normalized inbound message, picking the
// direct or conversation rule from the platform kind the receive loop observed.
func (c Config) allowsInbound(norm normalizedInbound) bool {
	if norm.kind == kindP2P {
		return c.allowsDirect(norm.msg.Source.SenderID)
	}
	return c.allowsConversation(norm.kind, norm.msg.Source.ChatID, norm.msg.Source.SenderID)
}

// conversationCandidates returns the ids one group/channel conversation can be
// matched against, most specific first.
//
// For a teams conversation that is BOTH the exact composite chat id and the
// channel-scoped one without its thread segment. The composite carries a thread
// id (every top-level post opens its own session, see composeTeamsChatID), so
// exact matching alone would need a new allowlist entry per post — which is also
// what the route inventory offers: channel_chat_session_binding stores the
// thread-scoped key, one row per post. The channel-scoped key is the granularity
// "allow this channel" actually means, so it matches too. A plain group has one
// id and gets one candidate. A malformed composite (an id that does not split
// into the documented shape) contributes only itself — never a guessed prefix
// match — so the "teams_" routing scheme stays authoritative.
func conversationCandidates(kind chatKind, chatID string) []string {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return nil
	}
	if kind != kindTeams {
		return []string{chatID}
	}
	team, channelID, _, ok := splitTeamsChatID(chatID)
	if !ok {
		return []string{chatID}
	}
	exact, conversation := chatID, composeTeamsChatID(team, channelID, "")
	if conversation == exact {
		return []string{exact}
	}
	return []string{exact, conversation}
}

// allowlistAuditKey is the id recorded for a refused conversation: the
// channel-scoped key for teams (precisely what the operator has to paste to let
// that channel in, and stable across its threads), the plain chat id otherwise.
// Rejections listed back to the Settings panel are deduplicated by it.
func allowlistAuditKey(kind chatKind, chatID string) string {
	candidates := conversationCandidates(kind, chatID)
	if len(candidates) == 0 {
		return ""
	}
	return candidates[len(candidates)-1]
}

// recordAllowlistDrop audits one refused message. The row carries the allowlist
// key rather than the raw chat id (see allowlistAuditKey) so what the panel
// offers back is exactly the entry that would let the sender in. Nothing here
// stores message content: the shared channel_inbound_audit surface holds
// routing, platform ids, and a reason only — and the reply the sender does NOT
// get is deliberate, matching the reference, which logs and discards.
func (c *tuituiChannel) recordAllowlistDrop(ctx context.Context, norm normalizedInbound) {
	if c.audit == nil || !c.instID.Valid {
		return
	}
	msg := norm.msg
	msg.Source.ChatID = allowlistAuditKey(norm.kind, msg.Source.ChatID)
	if err := c.audit.RecordDrop(ctx, c.instID, msg, engine.DropReason(AllowlistReason)); err != nil {
		c.logger.WarnContext(ctx, "tuitui: allowlist drop audit write failed",
			"error", err, "event_id", norm.msg.EventID)
	}
}

// allowlistSubject names the refused party for the log line: the id a list
// would have to carry, plus which list it belongs to.
type allowlistSubject struct {
	// ID is a user account for a refused DM, or the channel-scoped
	// conversation key for a refused group / teams message.
	ID string
	// Kind is the platform event shape behind the refusal
	// (single_chat / group_chat / teams_post_create / teams_post_modify):
	// the same label the audit row stores, and what tells a person from a
	// group when the panel reads the rows back.
	Kind string
}

func allowlistSubjectOf(norm normalizedInbound) allowlistSubject {
	return allowlistSubject{
		ID:   allowlistAuditKey(norm.kind, norm.msg.Source.ChatID),
		Kind: dropEventType(norm.msg),
	}
}
