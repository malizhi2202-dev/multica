package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/tuitui"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Tuitui (推推) is the IM chat channel shaped exactly like the existing four:
// a bring-your-own-app install per agent, one outbound WebSocket long
// connection per installation, and the same group / account-binding management
// surface as DingTalk. Every handler here mirrors its DingTalk twin — the same
// authorization (member-visible listing, agent-owner-or-admin install / revoke /
// forget), the same wire shape, and the same 200-with-configured:false answer
// when no master key resolves at all (see secretbox.ResolveIntegrationKey).

// TuituiInstallationResponse is the wire shape for a Tuitui installation row.
// The encrypted app_secret in config is INTENTIONALLY absent — it is
// server-internal (only the channel decrypts it). WS lease columns are runtime
// state, not API surface, so they are omitted too.
type TuituiInstallationResponse struct {
	ID              string `json:"id"`
	WorkspaceID     string `json:"workspace_id"`
	AgentID         string `json:"agent_id"`
	InstallerUserID string `json:"installer_user_id"`
	Status          string `json:"status"`
	InstalledAt     string `json:"installed_at"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	AgentAvailable  bool   `json:"agent_available"`
	// Host / Port echo the effective Tuitui server this bot dials, so the
	// user can confirm which server they connected. They resolve exactly what
	// the adapter falls back to at connect time (config.go FromInstallConfig):
	// a row stored without host/port — including every pre-address row and a
	// bare host without port — echoes the platform defaults, not a 0/blank.
	Host string `json:"host"`
	Port int    `json:"port"`
	// BoundTuituiUserIDs carries only the requesting member's own Tuitui
	// accounts for this bot, never other members'.
	BoundTuituiUserIDs []string `json:"bound_tuitui_user_ids,omitempty"`
	// AllowFromMode / GroupAllowFromMode echo the EFFECTIVE inbound gates and
	// AllowFrom / GroupAllowFrom the ids behind a gate that is a list.
	// "unrestricted" means the bot answers everyone: either the operator
	// stored the "*" wildcard or this row predates the gate and stores no key
	// at all. Both behave alike, so the panel shows "*" for either.
	// "allowlist" means only the echoed ids may talk; the empty list that can
	// appear there is a deliberate "nobody", never a synonym of unset — which
	// is exactly why the two modes are separate values instead of one list.
	AllowFromMode      string   `json:"allow_from_mode"`
	AllowFrom          []string `json:"allow_from"`
	GroupAllowFromMode string   `json:"group_allow_from_mode"`
	GroupAllowFrom     []string `json:"group_allow_from"`
}

// TuituiGroupBotResponse identifies one connected Multica bot behind a Tuitui
// group conversation. AgentID is the product-facing identity. Tuitui exposes no
// readable-bot-name lookup, so BotName stays empty and BotIdentityIssue says
// why — the same field pair the client already renders for DingTalk.
type TuituiGroupBotResponse struct {
	InstallationID   string `json:"installation_id"`
	AgentID          string `json:"agent_id"`
	BotName          string `json:"bot_name"`
	BotIdentityIssue string `json:"bot_identity_issue"`
	LastActiveAt     string `json:"last_active_at,omitempty"`
}

// TuituiGroupResponse is one group conversation with every bot routed to it.
// Teams (channel) conversations carry no platform chat id, so ConversationID is
// the adapter's composed "teams_{team}_{channel}[_{thread}]" key.
type TuituiGroupResponse struct {
	ConversationID    string                   `json:"conversation_id"`
	ConversationTitle string                   `json:"conversation_title"`
	Bots              []TuituiGroupBotResponse `json:"bots"`
}

// ListTuituiGroupsResponse is the group inventory. Inactive pagination and
// app-wide bot identities stay absent: Tuitui records no presence history, so
// there is no out-of-window set to page through and no identity outside a
// conversation to report.
type ListTuituiGroupsResponse struct {
	Groups []TuituiGroupResponse `json:"groups"`
	// GroupDiscoverySupported tells clients this server answers the groups
	// routes at all. It is a capability of this build, not of the platform:
	// conversations appear only after the bot has been addressed in them,
	// because Tuitui has no endpoint that enumerates a bot's groups.
	GroupDiscoverySupported bool `json:"group_discovery_supported"`
}

const (
	// tuituiBotIdentityIssueNoLookup is the machine-readable reason the
	// readable bot identity is unavailable: this adapter never resolves a
	// display name, so the Settings row falls back to the agent's identity.
	tuituiBotIdentityIssueNoLookup = "identity_lookup_unavailable"

	// The two echo values of an inbound allowlist gate. See
	// TuituiInstallationResponse: "unrestricted" covers both the wildcard and
	// a row stored before the gate existed, "allowlist" names the ids.
	tuituiAllowlistModeUnrestricted = "unrestricted"
	tuituiAllowlistModeAllowlist    = "allowlist"

	// tuituiRejectionKindUser / tuituiRejectionKindGroup label a refused
	// subject for the panel, which needs to know which of the two inputs a
	// clicked id belongs in.
	tuituiRejectionKindUser  = "user"
	tuituiRejectionKindGroup = "group"
)

// tuituiAllowlistEcho renders one gate as the (mode, ids) pair the response
// carries. Entries are always a non-nil slice so a list-mode gate with nothing
// on it serializes as [] ("nobody"), never null — null would be an unparsed
// field on the client, and "no list" is what the mode already says.
func tuituiAllowlistEcho(list tuitui.Allowlist) (string, []string) {
	mode := tuituiAllowlistModeUnrestricted
	if list.Mode == tuitui.AllowlistSome || list.Mode == tuitui.AllowlistNone {
		mode = tuituiAllowlistModeAllowlist
	}
	entries := list.Entries
	if entries == nil {
		entries = []string{}
	}
	return mode, entries
}

// tuituiEffectiveServer reads the stored host/port keys back out of an
// installation config so every response can echo the server the bot dials.
// Unreadable or pre-address blobs resolve to the adapter's own defaults —
// the same resolution FromInstallConfig performs at connect time, not a
// second source of truth.
func tuituiEffectiveServer(raw []byte) (string, int) {
	host, port := "", 0
	var stored struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	// A torn or legacy blob simply falls through to the defaults; the echo
	// must never fail a listing.
	if err := json.Unmarshal(raw, &stored); err == nil {
		host = strings.TrimSpace(stored.Host)
		port = stored.Port
	}
	if host == "" {
		host = tuitui.DefaultHost
	}
	if port < 1 || port > 65535 {
		port = tuitui.DefaultPort
	}
	return host, port
}

func tuituiInstallationToResponse(row db.ChannelInstallation) TuituiInstallationResponse {
	host, port := tuituiEffectiveServer(row.Config)
	direct, group := tuitui.AllowlistsFromStoredConfig(row.Config)
	directMode, directEntries := tuituiAllowlistEcho(direct)
	groupMode, groupEntries := tuituiAllowlistEcho(group)
	return TuituiInstallationResponse{
		ID:                 uuidToString(row.ID),
		WorkspaceID:        uuidToString(row.WorkspaceID),
		AgentID:            uuidToString(row.AgentID),
		InstallerUserID:    uuidToString(row.InstallerUserID),
		Status:             row.Status,
		InstalledAt:        row.InstalledAt.Time.UTC().Format(time.RFC3339),
		CreatedAt:          row.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt:          row.UpdatedAt.Time.UTC().Format(time.RFC3339),
		AgentAvailable:     true,
		Host:               host,
		Port:               port,
		AllowFromMode:      directMode,
		AllowFrom:          directEntries,
		GroupAllowFromMode: groupMode,
		GroupAllowFrom:     groupEntries,
	}
}

func emptyTuituiGroupsResponse() ListTuituiGroupsResponse {
	return ListTuituiGroupsResponse{
		Groups:                  []TuituiGroupResponse{},
		GroupDiscoverySupported: true,
	}
}

// tuituiAgentVisibility resolves the Agent side of the Settings inventory in one
// batch, mirroring dingtalkAgentVisibility. The available set distinguishes an
// orphaned installation from an inaccessible Agent for admins; the visible set
// enforces the same owner / role / invocation-target rules as the Agent list for
// ordinary members.
func (h *Handler) tuituiAgentVisibility(
	ctx context.Context,
	workspaceID pgtype.UUID,
	userID string,
	member db.Member,
) (available map[string]struct{}, visible map[string]struct{}, ok bool) {
	agents, err := h.Queries.ListAllAgents(ctx, workspaceID)
	if err != nil {
		return nil, nil, false
	}
	available = make(map[string]struct{}, len(agents))
	visible = make(map[string]struct{}, len(agents))
	for _, agent := range agents {
		agentID := uuidToString(agent.ID)
		available[agentID] = struct{}{}
		if roleAllowed(member.Role, "owner", "admin") {
			visible[agentID] = struct{}{}
		}
	}
	if roleAllowed(member.Role, "owner", "admin") {
		return available, visible, true
	}
	targetsByAgent, loaded := h.loadInvocationTargetsByAgent(ctx, agents)
	if !loaded {
		return nil, nil, false
	}
	for _, agent := range agents {
		agentID := uuidToString(agent.ID)
		if memberAllowedToViewAgent(agent, targetsByAgent[agentID], userID, member.Role) {
			visible[agentID] = struct{}{}
		}
	}
	return available, visible, true
}

// ListTuituiInstallations (GET /api/workspaces/{id}/tuitui/installations) is
// member-visible so the Integrations tab renders for non-admins. configured is
// the at-rest encryption key flag (TuituiInstall != nil) — the same derivation
// DingTalk uses.
func (h *Handler) ListTuituiInstallations(w http.ResponseWriter, r *http.Request) {
	if h.TuituiInstall == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"installations": []TuituiInstallationResponse{},
			"configured":    false,
		})
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	member, ok := h.workspaceMember(w, r, uuidToString(wsUUID))
	if !ok {
		return
	}
	availableAgentIDs, visibleAgentIDs, loaded := h.tuituiAgentVisibility(
		r.Context(), wsUUID, userID, member,
	)
	if !loaded {
		writeError(w, http.StatusInternalServerError, "failed to resolve tuitui installation visibility")
		return
	}
	rows, err := h.TuituiInstall.ListByWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tuitui installations")
		return
	}
	bindingsByInstallation := map[pgtype.UUID][]string{}
	canViewAll := roleAllowed(member.Role, "owner", "admin")
	if canViewAll {
		userUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
		if !ok {
			return
		}
		bindings, err := h.Queries.ListTuituiUserBindingsForMember(r.Context(), db.ListTuituiUserBindingsForMemberParams{
			WorkspaceID:   wsUUID,
			MulticaUserID: userUUID,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list tuitui user bindings")
			return
		}
		for _, binding := range bindings {
			bindingsByInstallation[binding.InstallationID] = append(
				bindingsByInstallation[binding.InstallationID],
				binding.ChannelUserID,
			)
		}
	}
	out := make([]TuituiInstallationResponse, 0, len(rows))
	for _, row := range rows {
		agentID := uuidToString(row.AgentID)
		if !canViewAll {
			if _, canView := visibleAgentIDs[agentID]; !canView {
				continue
			}
		}
		response := tuituiInstallationToResponse(row)
		_, response.AgentAvailable = availableAgentIDs[agentID]
		response.BoundTuituiUserIDs = bindingsByInstallation[row.ID]
		out = append(out, response)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"installations": out,
		"configured":    true,
	})
}

// ListTuituiGroups (GET /api/workspaces/{id}/tuitui/groups) returns the Settings
// inventory of group conversations this workspace's Tuitui bots are routed
// into. Workspace owners/admins see the full inventory; ordinary members only
// receive bots for Agents they can open, matching ListAgents and Agent Detail.
func (h *Handler) ListTuituiGroups(w http.ResponseWriter, r *http.Request) {
	if h.TuituiInstall == nil {
		writeJSON(w, http.StatusOK, emptyTuituiGroupsResponse())
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	member, ok := h.workspaceMember(w, r, uuidToString(wsUUID))
	if !ok {
		return
	}
	var visibleAgentIDs map[string]struct{}
	if !roleAllowed(member.Role, "owner", "admin") {
		_, visible, loaded := h.tuituiAgentVisibility(r.Context(), wsUUID, userID, member)
		if !loaded {
			writeError(w, http.StatusInternalServerError, "failed to resolve tuitui group visibility")
			return
		}
		visibleAgentIDs = visible
	}
	h.listTuituiGroups(w, r, wsUUID, "", visibleAgentIDs)
}

// ListTuituiGroupsForAgent (GET /api/agents/{id}/tuitui/groups) exposes only the
// selected agent's observed groups. It deliberately reuses the Agent detail
// view gate: if the caller cannot open this Agent, they cannot infer its Tuitui
// group activity either.
func (h *Handler) ListTuituiGroupsForAgent(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "id")
	agent, ok := h.loadAgentForUser(w, r, agentID)
	if !ok {
		return
	}
	workspaceID := uuidToString(agent.WorkspaceID)
	actorType, actorID := h.resolveActor(r, requestUserID(r), workspaceID)
	if !h.canAccessPrivateAgent(r.Context(), agent, actorType, actorID, workspaceID) {
		writeError(w, http.StatusForbidden, "you do not have access to this agent")
		return
	}
	if h.TuituiInstall == nil {
		writeJSON(w, http.StatusOK, emptyTuituiGroupsResponse())
		return
	}
	h.listTuituiGroups(w, r, agent.WorkspaceID, agentID, nil)
}

// listTuituiGroups projects the live group session bindings of this workspace's
// active Tuitui installations into the Settings inventory. The activity /
// installation_id / offset / limit parameters DingTalk's inventory accepts are
// validated identically so a client built against that panel gets the same 400s,
// but the inactive page is always empty: Tuitui keeps no presence history, so
// there is nothing outside an active window to page through.
func (h *Handler) listTuituiGroups(
	w http.ResponseWriter,
	r *http.Request,
	workspaceID pgtype.UUID,
	agentID string,
	visibleAgentIDs map[string]struct{},
) {
	filterByAgent := agentID != ""
	var agentUUID pgtype.UUID
	if filterByAgent {
		agentUUID = parseUUID(agentID)
	}
	activity := strings.TrimSpace(r.URL.Query().Get("activity"))
	if activity != "" && activity != "inactive" {
		writeError(w, http.StatusBadRequest, "activity must be inactive when provided")
		return
	}
	includeInactive := activity == "inactive"
	installationID := strings.TrimSpace(r.URL.Query().Get("installation_id"))
	if includeInactive && installationID == "" {
		writeError(w, http.StatusBadRequest, "installation_id is required for inactive groups")
		return
	}
	if installationID != "" {
		if _, ok := parseUUIDOrBadRequest(w, installationID, "installation_id"); !ok {
			return
		}
	}
	if includeInactive {
		writeJSON(w, http.StatusOK, emptyTuituiGroupsResponse())
		return
	}
	rows, err := h.Queries.ListTuituiGroupSessionsByWorkspace(r.Context(), db.ListTuituiGroupSessionsByWorkspaceParams{
		WorkspaceID:   workspaceID,
		FilterByAgent: filterByAgent,
		AgentID:       agentUUID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tuitui groups")
		return
	}
	groupIndexByID := make(map[string]int, len(rows))
	groups := make([]TuituiGroupResponse, 0, len(rows))
	for _, row := range rows {
		rowAgentID := uuidToString(row.AgentID)
		if visibleAgentIDs != nil {
			if _, visible := visibleAgentIDs[rowAgentID]; !visible {
				continue
			}
		}
		lastActiveAt := ""
		if row.UpdatedAt.Valid {
			lastActiveAt = row.UpdatedAt.Time.UTC().Format(time.RFC3339)
		}
		bot := TuituiGroupBotResponse{
			InstallationID:   uuidToString(row.InstallationID),
			AgentID:          rowAgentID,
			BotIdentityIssue: tuituiBotIdentityIssueNoLookup,
			LastActiveAt:     lastActiveAt,
		}
		if groupIndex, exists := groupIndexByID[row.ChannelChatID]; exists {
			groups[groupIndex].Bots = append(groups[groupIndex].Bots, bot)
			if groups[groupIndex].ConversationTitle == "" && row.Title != "" {
				groups[groupIndex].ConversationTitle = row.Title
			}
			continue
		}
		groups = append(groups, TuituiGroupResponse{
			ConversationID:    row.ChannelChatID,
			ConversationTitle: row.Title,
			Bots:              []TuituiGroupBotResponse{bot},
		})
		groupIndexByID[row.ChannelChatID] = len(groups) - 1
	}
	for index := range groups {
		sort.SliceStable(groups[index].Bots, func(i, j int) bool {
			return groups[index].Bots[i].InstallationID < groups[index].Bots[j].InstallationID
		})
	}
	sort.SliceStable(groups, func(i, j int) bool {
		left, right := groups[i].ConversationTitle, groups[j].ConversationTitle
		if left == right {
			return groups[i].ConversationID < groups[j].ConversationID
		}
		if left == "" {
			return false
		}
		if right == "" {
			return true
		}
		return left < right
	})
	writeJSON(w, http.StatusOK, ListTuituiGroupsResponse{
		Groups:                  groups,
		GroupDiscoverySupported: true,
	})
}

// ForgetTuituiGroup (DELETE
// /api/workspaces/{id}/tuitui/installations/{installationId}/groups/{conversationId})
// retires one group's live route. The chat session, its history, and the
// installation are all retained; the next successfully addressed message from
// the same group opens a fresh session, which puts it back in the inventory.
// That is the closest available analogue of DingTalk's presence forget: Tuitui
// has no separate observation table to clear.
func (h *Handler) ForgetTuituiGroup(w http.ResponseWriter, r *http.Request) {
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	if _, ok := h.requireWorkspaceRole(w, r, uuidToString(wsUUID), "tuitui group not found", "owner", "admin"); !ok {
		return
	}
	installationUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "installationId"), "installation id")
	if !ok {
		return
	}
	conversationID, err := url.PathUnescape(chi.URLParam(r, "conversationId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid conversation id")
		return
	}
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		writeError(w, http.StatusBadRequest, "conversation id is required")
		return
	}
	if _, err = h.Queries.ForgetTuituiGroupSession(r.Context(), db.ForgetTuituiGroupSessionParams{
		WorkspaceID:    wsUUID,
		InstallationID: installationUUID,
		ChannelChatID:  conversationID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "tuitui group not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to forget tuitui group")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// TuituiRejectionResponse is one subject a Tuitui bot refused because no
// allowlist named it: the id to paste, which of the two lists it belongs in,
// and how recently / how often it was turned away. Content stays out of this —
// the audit table it reads holds routing facts only.
type TuituiRejectionResponse struct {
	// ID is a Tuitui user account (Kind user) or a group id / channel-scoped
	// "teams_{team}_{channel}" conversation key (Kind group).
	ID string `json:"id"`
	// Kind is "user" or "group" — which input the panel adds the id to.
	Kind string `json:"kind"`
	// InstallationID / AgentID attribute the refusal to one connected bot, so
	// a workspace with several bots never mixes their lists into one suggestion.
	InstallationID string `json:"installation_id"`
	AgentID        string `json:"agent_id"`
	// Count is how many messages from this subject were refused, and LastSeenAt
	// when the newest one arrived. Both only ever grow while the bot is
	// connected, which is what tells "typing this id in" from "guessing".
	Count      int64  `json:"count"`
	LastSeenAt string `json:"last_seen_at"`
}

// ListTuituiRejectionsResponse wraps the page. Empty is a normal answer (an
// unconfigured gate refuses nobody, so nothing is pending), never an error.
// NextOffset is present when the page read from the audit table came back full
// — which can happen with fewer or even zero visible rows, because a non-admin's
// visibility filter drops rows after the read. It therefore means "there may be
// more, keep paging", never "this was truncated for you".
type ListTuituiRejectionsResponse struct {
	Rejections []TuituiRejectionResponse `json:"rejections"`
	NextOffset *int                      `json:"next_offset,omitempty"`
}

const (
	// tuituiRejectionsDefaultLimit / tuituiRejectionsMaxLimit bound one page of
	// the refused-subject list. The panel shows a first screenful and offers
	// more; the cap keeps a chatty refused group from turning a settings read
	// into an unbounded aggregate scan.
	tuituiRejectionsDefaultLimit = 50
	tuituiRejectionsMaxLimit     = 200
)

// ListTuituiRejections (GET /api/workspaces/{id}/tuitui/rejections) backs the
// "recently refused" picker in the allowlist dialog. Reading is gated exactly
// like the rest of this channel: workspace membership (the route middleware)
// plus, for a non-admin, the same agent-visibility filter the group inventory
// applies — a member only sees refusals attributed to bots they may open.
//
// Optional query parameters: installation_id (one bot) and limit / offset.
func (h *Handler) ListTuituiRejections(w http.ResponseWriter, r *http.Request) {
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	member, ok := h.workspaceMember(w, r, uuidToString(wsUUID))
	if !ok {
		return
	}
	// The gate that produced these rows lives in the channel, which only runs
	// when a master key resolved at boot. Without one nothing can be refused,
	// so the honest answer is an empty page rather than a 403: the dialog
	// simply shows no suggestions, like a bot nobody has tried to message.
	if h.TuituiInstall == nil {
		writeJSON(w, http.StatusOK, ListTuituiRejectionsResponse{Rejections: []TuituiRejectionResponse{}})
		return
	}
	installationID := strings.TrimSpace(r.URL.Query().Get("installation_id"))
	var installationUUID pgtype.UUID
	if installationID != "" {
		parsed, ok := parseUUIDOrBadRequest(w, installationID, "installation_id")
		if !ok {
			return
		}
		installationUUID = parsed
	}
	limit, ok := parseTuituiRejectionLimit(w, r.URL.Query().Get("limit"))
	if !ok {
		return
	}
	offset, ok := parseTuituiRejectionOffset(w, r.URL.Query().Get("offset"))
	if !ok {
		return
	}
	var visibleAgentIDs map[string]struct{}
	if !roleAllowed(member.Role, "owner", "admin") {
		_, visible, loaded := h.tuituiAgentVisibility(r.Context(), wsUUID, userID, member)
		if !loaded {
			writeError(w, http.StatusInternalServerError, "failed to resolve tuitui rejection visibility")
			return
		}
		visibleAgentIDs = visible
	}
	rows, err := h.Queries.ListTuituiAllowlistRejections(r.Context(), db.ListTuituiAllowlistRejectionsParams{
		DropReason:           tuitui.AllowlistReason,
		WorkspaceID:          wsUUID,
		FilterByInstallation: installationID != "",
		InstallationID:       installationUUID,
		RowLimit:             int32(limit),
		RowOffset:            int32(offset),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tuitui rejections")
		return
	}
	out := make([]TuituiRejectionResponse, 0, len(rows))
	for _, row := range rows {
		agentID := uuidToString(row.AgentID)
		if visibleAgentIDs != nil {
			if _, visible := visibleAgentIDs[agentID]; !visible {
				continue
			}
		}
		// A subject id is only meaningful as text: an audit row written before
		// the chat id existed (channel_chat_id NULL) is filtered in SQL, so an
		// invalid NULL here would be a bug, not data to render.
		subjectID := ""
		if row.SubjectID.Valid {
			subjectID = row.SubjectID.String
		}
		if subjectID == "" {
			continue
		}
		kind := tuituiRejectionKindGroup
		if row.IsDirect {
			kind = tuituiRejectionKindUser
		}
		lastSeen := ""
		if row.LastSeenAt.Valid {
			lastSeen = row.LastSeenAt.Time.UTC().Format(time.RFC3339)
		}
		out = append(out, TuituiRejectionResponse{
			ID:             subjectID,
			Kind:           kind,
			InstallationID: uuidToString(row.InstallationID),
			AgentID:        agentID,
			Count:          row.SeenCount,
			LastSeenAt:     lastSeen,
		})
	}
	response := ListTuituiRejectionsResponse{Rejections: out}
	if len(rows) == limit {
		next := offset + limit
		response.NextOffset = &next
	}
	writeJSON(w, http.StatusOK, response)
}

// parseTuituiRejectionLimit reads ?limit= with a default and a clamp: a
// non-numeric or negative value is the caller's 400, an oversized one is
// capped, because the cap protects the reader rather than meaning anything to
// the dialog.
func parseTuituiRejectionLimit(w http.ResponseWriter, raw string) (int, bool) {
	if strings.TrimSpace(raw) == "" {
		return tuituiRejectionsDefaultLimit, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		writeError(w, http.StatusBadRequest, "limit must be a positive integer")
		return 0, false
	}
	if n > tuituiRejectionsMaxLimit {
		n = tuituiRejectionsMaxLimit
	}
	return n, true
}

// parseTuituiRejectionOffset reads ?offset= the same way, minus the clamp: past
// the end of the list there is simply nothing to page through.
func parseTuituiRejectionOffset(w http.ResponseWriter, raw string) (int, bool) {
	if strings.TrimSpace(raw) == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		writeError(w, http.StatusBadRequest, "offset must be zero or a positive integer")
		return 0, false
	}
	return n, true
}

// RegisterTuituiBYORequest is the body for a bring-your-own-app install: the
// address of the Tuitui server to connect to plus the two credentials the user
// pasted from their own Tuitui robot application. The inbound allowlists are NOT
// part of it: connecting a bot never restricts it (see the "unrestricted"
// default in allow.go), and the gate has its own narrow write —
// UpdateTuituiAllowlists — because this endpoint replaces the whole row and
// would otherwise demand the app_secret again just to add one group.
type RegisterTuituiBYORequest struct {
	// BaseURL is the Tuitui server Multica must connect to: "https://host:port",
	// "wss://host", or a bare "host[:port]". The Tuitui WSS callback and HTTPS
	// API share one host:port (see integrations/tuitui/config.go), so the
	// dialog collects a single address instead of two duplicate fields; this
	// handler resolves it into the host + port pair stored under
	// config->>'host' / config->>'port'. The scheme is only used to interpret
	// the input — the adapter builds the wss/https URLs itself.
	BaseURL   string `json:"base_url"`
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
}

// tuituiServerAddressSchemes are the only schemes accepted for the BYO server
// address; anything else is a typo or a different protocol.
var tuituiServerAddressSchemes = map[string]struct{}{
	"http": {}, "https": {}, "ws": {}, "wss": {},
}

// parseTuituiServerAddress resolves the single "server address" field the BYO
// dialog collects into the host + port pair the adapter stores. Accepted forms:
// "https://host:port", "wss://host", or a bare "host:port" / "host". The
// Tuitui WSS callback and HTTPS API share one host:port (see
// integrations/tuitui/config.go), so a path, query, fragment, or userinfo is
// rejected — the address is host and optional port only; the adapter composes
// the fixed API/callback paths. One exception normalizes away: a single
// trailing "/" ("https://host:8282/") carries no information and is what
// browsers and ops docs paste; anything slash-beyond-it ("//", "/a/") is path
// semantics and stays refused. A bare host without a port returns port 0 so
// the adapter applies its own DefaultPort; the handler must not encode that
// default here.
func parseTuituiServerAddress(raw string) (host string, port int, err error) {
	address := strings.TrimSpace(raw)
	if address == "" {
		return "", 0, errors.New(
			"server address is required — enter the Tuitui server to connect to, e.g. https://tuitui.internal:8282")
	}
	if !strings.Contains(address, "://") {
		// Bare authority. url.Parse would read "host:8282" as scheme "host",
		// so anything path/query/fragment-shaped is refused outright and the
		// rest is parsed under a placeholder scheme for uniform extraction.
		if strings.ContainsAny(address, "/?#\\") {
			return "", 0, fmt.Errorf(
				"invalid server address %q — give only the host and optional port, e.g. tuitui.internal:8282", address)
		}
		address = "https://" + address
	}
	u, parseErr := url.Parse(address)
	if parseErr != nil {
		return "", 0, fmt.Errorf("invalid server address: %v", parseErr)
	}
	if _, ok := tuituiServerAddressSchemes[strings.ToLower(u.Scheme)]; !ok {
		return "", 0, fmt.Errorf(
			"unsupported scheme %q in server address — use http, https, ws or wss, or just host:port", u.Scheme)
	}
	if u.Host == "" {
		return "", 0, errors.New("invalid server address: the host is missing")
	}
	// A single trailing slash is normalized away — it carries no path
	// information; anything beyond that ("//", "/a/") is path semantics and
	// stays 400.
	if u.Path != "" && u.Path != "/" {
		return "", 0, fmt.Errorf(
			"invalid server address: a path is not allowed (the server is dialed on fixed paths) — remove %q", u.Path)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", 0, errors.New("invalid server address: a query string is not allowed")
	}
	if u.Fragment != "" {
		return "", 0, errors.New("invalid server address: a fragment is not allowed")
	}
	if u.User != nil {
		return "", 0, errors.New(
			"invalid server address: username/password in the address are not allowed — credentials belong in the AppID and AppSecret fields")
	}
	host = u.Hostname()
	if host == "" {
		return "", 0, errors.New("invalid server address: the host is missing")
	}
	if p := u.Port(); p != "" {
		n, convErr := strconv.Atoi(p)
		if convErr != nil || n < 1 || n > 65535 {
			return "", 0, fmt.Errorf("invalid server address: port %q is not between 1 and 65535", p)
		}
		port = n
	}
	// url.Parse strips the brackets every consumer (the wss dial string, the
	// https host:port) requires around an IPv6 literal; put them back so the
	// adapter's "wss://{host}:{port}" composition stays dialable.
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return host, port, nil
}

// tuituiAllowlistFromField turns one optional field of the allowlist write into
// the value to store. nil (the client said nothing about this list) leaves the
// stored key alone; a pointer to an empty string is the operator clearing the
// box, which blocks everyone. Those two must not collapse, or the panel would
// have no way to leave one list alone while editing the other.
func tuituiAllowlistFromField(field *string) (*tuitui.Allowlist, error) {
	if field == nil {
		return nil, nil
	}
	list, err := tuitui.AllowlistFromCSV(*field)
	if err != nil {
		return nil, err
	}
	return &list, nil
}

// RegisterTuituiBYO (POST /api/workspaces/{id}/tuitui/install/byo?agent_id=…)
// installs a user-supplied ("bring your own") Tuitui robot for an agent, so
// several agents can each have their own bot identity in the SAME Tuitui
// organization. The router requires workspace membership; this handler then
// authorizes the target agent's owner or a workspace owner/admin. Like DingTalk's
// BYO path this needs only a master key that resolved at boot (TuituiInstall
// != nil): the secret is sealed with it before the row is written.
func (h *Handler) RegisterTuituiBYO(w http.ResponseWriter, r *http.Request) {
	if h.TuituiInstall == nil {
		writeFeatureDisabled(w, "tuitui_not_configured", "tuitui integration not enabled")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	agentIDStr := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	if agentIDStr == "" {
		writeError(w, http.StatusBadRequest, "agent_id is required")
		return
	}
	agentUUID, ok := parseUUIDOrBadRequest(w, agentIDStr, "agent_id")
	if !ok {
		return
	}
	// Resolve and authorize the target agent at the boundary so a wrong agent_id
	// is a clear 404 and an unrelated member cannot connect a bot to it.
	agent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
		ID:          agentUUID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "agent not found in this workspace")
		return
	}
	if !h.canManageAgent(w, r, agent) {
		return
	}
	initiatorUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	var body RegisterTuituiBYORequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Resolve the single server-address field at the boundary: a bad or missing
	// address is a 400 the dialog can show, never a silent fallback to the
	// public cloud default and never a 500.
	host, port, addrErr := parseTuituiServerAddress(body.BaseURL)
	if addrErr != nil {
		writeError(w, http.StatusBadRequest, addrErr.Error())
		return
	}
	row, err := h.TuituiInstall.RegisterBYO(r.Context(), tuitui.RegisterBYOParams{
		WorkspaceID: wsUUID,
		AgentID:     agentUUID,
		InitiatorID: initiatorUUID,
		Host:        host,
		Port:        port,
		AppID:       body.AppID,
		AppSecret:   body.AppSecret,
	})
	if err != nil {
		switch {
		case errors.Is(err, tuitui.ErrInvalidAppID), errors.Is(err, tuitui.ErrInvalidAppSecret),
			errors.Is(err, tuitui.ErrInvalidHost):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, tuitui.ErrAppOwnedBySameWorkspace):
			writeError(w, http.StatusConflict, "this Tuitui app is already connected to another agent in this workspace — disconnect it there first, then connect it here")
		case errors.Is(err, tuitui.ErrAppOwnedByArchivedAgent):
			writeError(w, http.StatusConflict, "this Tuitui app is connected to an archived agent in this workspace — restore that agent, or disconnect its bot, before connecting it here")
		case errors.Is(err, tuitui.ErrAppOwnedByAnotherWorkspace):
			writeError(w, http.StatusConflict, "this Tuitui app is already connected to a different Multica workspace — disconnect it there before connecting it here")
		default:
			// Encrypt / persist / unexpected failures are server-side, not the
			// user's credentials — surface a 500 instead of misreporting them as
			// bad credentials.
			writeError(w, http.StatusInternalServerError, "could not connect the Tuitui bot")
		}
		return
	}
	// Broadcast so every open client (Settings, Agent Integrations, other tabs)
	// invalidates its installations query and shows the new bot — matching the
	// revoke event and DingTalk / Slack's install semantics.
	h.publishTuituiInstallationCreated(row, userID)
	writeJSON(w, http.StatusOK, tuituiInstallationToResponse(row))
}

// publishTuituiInstallationCreated emits tuitui_installation:created for a newly
// connected bot. The realtime layer fans it out to the workspace; the web app
// listens on tuitui_installation:* to invalidate the installations query.
func (h *Handler) publishTuituiInstallationCreated(row db.ChannelInstallation, actorID string) {
	h.publish(protocol.EventTuituiInstallationCreated, uuidToString(row.WorkspaceID), "user", actorID, map[string]any{
		"id": uuidToString(row.ID),
	})
}

// RevokeTuituiInstallation (DELETE /api/workspaces/{id}/tuitui/installations/{installationId})
// flips status to 'revoked'. The router requires workspace membership; this
// handler authorizes the bound agent's owner or a workspace owner/admin. The row
// is preserved for audit; a re-install (re-pasting the app's credentials) flips
// status back to 'active'. An orphaned installation falls back to workspace
// owner/admin-only cleanup because there is no agent owner to resolve.
func (h *Handler) RevokeTuituiInstallation(w http.ResponseWriter, r *http.Request) {
	if h.TuituiInstall == nil {
		writeFeatureDisabled(w, "tuitui_not_configured", "tuitui integration not configured")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	instUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "installationId"), "installation id")
	if !ok {
		return
	}
	// Workspace-scoped lookup so one workspace cannot revoke another's
	// installation by guessing the UUID.
	inst, err := h.TuituiInstall.GetInWorkspace(r.Context(), instUUID, wsUUID)
	if err != nil {
		if errors.Is(err, tuitui.ErrInstallationNotFound) {
			writeError(w, http.StatusNotFound, "tuitui installation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load installation")
		return
	}
	agent, agentErr := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
		ID:          inst.AgentID,
		WorkspaceID: wsUUID,
	})
	if agentErr != nil {
		if _, ok := h.requireWorkspaceRole(w, r, uuidToString(wsUUID), "tuitui installation not found", "owner", "admin"); !ok {
			return
		}
	} else if !h.canManageAgent(w, r, agent) {
		return
	}
	if err := h.TuituiInstall.Revoke(r.Context(), instUUID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to revoke installation")
		return
	}
	h.publish(protocol.EventTuituiInstallationRevoked, uuidToString(wsUUID), "user", userID, map[string]any{
		"id": uuidToString(instUUID),
	})
	w.WriteHeader(http.StatusNoContent)
}

// UpdateTuituiAllowlistsRequest is the whole body of the narrow allowlist
// write. Both fields are optional in the one sense that matters: a key the
// client omits leaves that list exactly as stored, so the dialog can tighten the
// group list without resending the personal one. A key sent as an empty string
// is NOT "no opinion" — it is the box the operator cleared, which stores [] and
// blocks everyone. The two are only distinguishable because one is absent and
// the other is present-but-empty, which is why both fields are pointers.
type UpdateTuituiAllowlistsRequest struct {
	AllowFrom      *string `json:"allow_from,omitempty"`
	GroupAllowFrom *string `json:"group_allow_from,omitempty"`
}

// UpdateTuituiAllowlists (PUT /api/workspaces/{id}/tuitui/installations/{installationId}/allowlists)
// replaces one bot's inbound allowlists and echoes the installation back with
// the effective gates.
//
// This is deliberately NOT a second submit of the BYO form. That endpoint
// replaces the whole installation and requires app_id + app_secret, which the
// panel never holds (the secret is sealed at rest and never echoed, and both
// inputs are masked), so "add one group to the allowlist" would demand pasting
// the credential again — and pasting it wrong would break a working bot. Here
// only the two config keys are written, in place, by one SQL statement that
// merges them into the stored blob (see SetTuituiInstallationAllowlists).
//
// Authorization mirrors RevokeTuituiInstallation exactly: the same person who
// connected the bot (its agent's manager) or a workspace owner/admin may
// restrict it, and a workspace-scoped lookup keeps a forged id in another
// workspace at 404 rather than leaking that the row exists.
func (h *Handler) UpdateTuituiAllowlists(w http.ResponseWriter, r *http.Request) {
	if h.TuituiInstall == nil {
		writeFeatureDisabled(w, "tuitui_not_configured", "tuitui integration not configured")
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	instUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "installationId"), "installation id")
	if !ok {
		return
	}
	inst, err := h.TuituiInstall.GetInWorkspace(r.Context(), instUUID, wsUUID)
	if err != nil {
		if errors.Is(err, tuitui.ErrInstallationNotFound) {
			writeError(w, http.StatusNotFound, "tuitui installation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load installation")
		return
	}
	agent, agentErr := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
		ID:          inst.AgentID,
		WorkspaceID: wsUUID,
	})
	if agentErr != nil {
		if _, ok := h.requireWorkspaceRole(w, r, uuidToString(wsUUID), "tuitui installation not found", "owner", "admin"); !ok {
			return
		}
	} else if !h.canManageAgent(w, r, agent) {
		return
	}
	var body UpdateTuituiAllowlistsRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Normalized here, once: trimmed, de-duplicated, and validated, so the gate
	// never has to re-interpret a stored entry and a typo is a 400 naming the
	// field that holds it rather than a silently inert list.
	allowFrom, allowErr := tuituiAllowlistFromField(body.AllowFrom)
	if allowErr != nil {
		writeError(w, http.StatusBadRequest, "allow_from: "+allowErr.Error())
		return
	}
	groupAllowFrom, groupErr := tuituiAllowlistFromField(body.GroupAllowFrom)
	if groupErr != nil {
		writeError(w, http.StatusBadRequest, "group_allow_from: "+groupErr.Error())
		return
	}
	// A revoked bot is not supervised, so its gate decides nothing: writing it
	// would read as "I restricted that bot" while the row still shows as
	// disconnected. Refuse rather than park a change nothing will honour.
	if inst.Status != "active" {
		writeError(w, http.StatusConflict, "tuitui installation is not active")
		return
	}
	updated, err := h.Queries.SetTuituiInstallationAllowlists(r.Context(), db.SetTuituiInstallationAllowlistsParams{
		AllowFrom:      tuituiAllowlistStoredJSON(allowFrom),
		GroupAllowFrom: tuituiAllowlistStoredJSON(groupAllowFrom),
		ID:             instUUID,
		WorkspaceID:    wsUUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "tuitui installation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update tuitui allowlists")
		return
	}
	// The response is the same echo the list renders, so the dialog can show
	// what the gate now IS (an entry the operator typed but the normalizer
	// dropped, or a list that collapsed to the wildcard, is visible immediately)
	// without a second round trip.
	writeJSON(w, http.StatusOK, tuituiInstallationToResponse(updated))
}

// tuituiAllowlistStoredJSON is the jsonb value for one argument of the in-place
// update: the list to store, or nil to leave that key alone.
func tuituiAllowlistStoredJSON(list *tuitui.Allowlist) []byte {
	if list == nil {
		return nil
	}
	stored := list.Stored()
	if stored == nil {
		// Unreachable from this endpoint (a nil request field is handled
		// above), and the safe reading anyway: no opinion, no write.
		return nil
	}
	// A list of strings cannot fail to marshal, so no error path is carried.
	raw, _ := json.Marshal(*stored)
	return raw
}

// RedeemTuituiBindingTokenRequest carries the raw token the user clicked through
// from the bot's "link your account" prompt.
type RedeemTuituiBindingTokenRequest struct {
	Token string `json:"token"`
}

// RedeemTuituiBindingTokenResponse echoes the bound workspace / installation /
// user so the frontend can confirm without a second fetch.
type RedeemTuituiBindingTokenResponse struct {
	WorkspaceID    string `json:"workspace_id"`
	InstallationID string `json:"installation_id"`
	TuituiUserID   string `json:"tuitui_user_id"`
}

// RedeemTuituiBindingToken (POST /api/tuitui/binding/redeem) binds the Tuitui
// account carried by the bearer token to the logged-in Multica user. The
// redeemer's identity comes from the session, while token possession proves
// control of the link delivered to that Tuitui account. Failure modes map to
// distinct status codes:
//   - 410 Gone:      token unknown / consumed / expired
//   - 409 Conflict:  this Tuitui account is already bound to a different user
//   - 403 Forbidden: redeemer is not a workspace member
func (h *Handler) RedeemTuituiBindingToken(w http.ResponseWriter, r *http.Request) {
	if h.TuituiBindingTokens == nil {
		writeFeatureDisabled(w, "tuitui_not_configured", "tuitui integration not configured")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var req RedeemTuituiBindingTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	userUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	redeemed, err := h.TuituiBindingTokens.RedeemAndBind(r.Context(), req.Token, userUUID)
	if err != nil {
		switch {
		case errors.Is(err, tuitui.ErrBindingTokenInvalid):
			writeError(w, http.StatusGone, "binding token invalid or expired")
		case errors.Is(err, tuitui.ErrBindingAlreadyAssigned):
			writeError(w, http.StatusConflict, "this Tuitui account is already bound to a different Multica user")
		case errors.Is(err, tuitui.ErrBindingNotWorkspaceMember):
			writeError(w, http.StatusForbidden, "binding refused (are you a workspace member?)")
		default:
			writeError(w, http.StatusInternalServerError, "failed to redeem token")
		}
		return
	}
	h.publish(protocol.EventTuituiAccountBindingUpdated, uuidToString(redeemed.WorkspaceID), "user", userID, map[string]any{
		"id": uuidToString(redeemed.InstallationID),
	})
	writeJSON(w, http.StatusOK, RedeemTuituiBindingTokenResponse{
		WorkspaceID:    uuidToString(redeemed.WorkspaceID),
		InstallationID: uuidToString(redeemed.InstallationID),
		TuituiUserID:   redeemed.TuituiUserID,
	})
}
