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
)

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
	return TuituiInstallationResponse{
		ID:              uuidToString(row.ID),
		WorkspaceID:     uuidToString(row.WorkspaceID),
		AgentID:         uuidToString(row.AgentID),
		InstallerUserID: uuidToString(row.InstallerUserID),
		Status:          row.Status,
		InstalledAt:     row.InstalledAt.Time.UTC().Format(time.RFC3339),
		CreatedAt:       row.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt:       row.UpdatedAt.Time.UTC().Format(time.RFC3339),
		AgentAvailable:  true,
		Host:            host,
		Port:            port,
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

// RegisterTuituiBYORequest is the body for a bring-your-own-app install: the
// address of the Tuitui server to connect to plus the two credentials the user
// pasted from their own Tuitui robot application.
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
