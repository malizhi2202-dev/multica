package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	tuituiintegration "github.com/multica-ai/multica/server/internal/integrations/tuitui"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util/secretbox"
)

// Tuitui handler tests. Every case mirrors the DingTalk surface this channel was
// cloned from: same authorization, same wire shape, and — because Tuitui keeps
// its app_secret in the shared channel_installation.config column — an explicit
// assertion that what reaches the row is ciphertext and never the pasted
// plaintext. RegisterBYO performs no credential probe against the platform, so
// nothing here can reach the network and no stub server is needed.

// tuituiTestKey is an all-zero 32-byte master key. Tests only seal and open
// their own fixtures with it, so it guards nothing; its length is what keeps the
// secretbox path identical to production's.
var tuituiTestKey = make([]byte, secretbox.KeySize)

// wireTuituiInstallService builds the real InstallService against the test
// database with a known key, so a test can read the stored config back and prove
// the secret was sealed. Both Tuitui services are restored afterwards.
func wireTuituiInstallService(t *testing.T) *secretbox.Box {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	box, err := secretbox.New(tuituiTestKey)
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	installSvc, err := tuituiintegration.NewInstallService(testHandler.Queries, testPool, box, nil)
	if err != nil {
		t.Fatalf("tuitui.NewInstallService: %v", err)
	}
	previousInstall := testHandler.TuituiInstall
	previousTokens := testHandler.TuituiBindingTokens
	testHandler.TuituiInstall = installSvc
	testHandler.TuituiBindingTokens = tuituiintegration.NewBindingTokenService(testHandler.Queries, testPool)
	t.Cleanup(func() {
		testHandler.TuituiInstall = previousInstall
		testHandler.TuituiBindingTokens = previousTokens
	})
	return box
}

// tuituiUniqueAppID keeps each test's bot identity its own: the
// (channel_type, config->>'app_id') routing index is workspace-agnostic, so one
// fixed id would make unrelated tests collide as 409 conflicts.
func tuituiUniqueAppID(prefix string) string {
	return prefix + "-" + strings.ToLower(uuid.NewString()[:8])
}

// tuituiBYORequest builds POST /api/workspaces/{id}/tuitui/install/byo?agent_id=…
func tuituiBYORequest(userID, agentID string, body any) *http.Request {
	path := "/api/workspaces/" + testWorkspaceID + "/tuitui/install/byo"
	if agentID != "" {
		path += "?agent_id=" + agentID
	}
	return withURLParams(newRequestAs(userID, http.MethodPost, path, body), "id", testWorkspaceID)
}

// tuituiInstallByo connects a bot with fresh credentials and registers the row
// for cleanup, returning its wire response.
func tuituiInstallByo(t *testing.T, userID, agentID string) TuituiInstallationResponse {
	t.Helper()
	out := testutil.Decode[TuituiInstallationResponse](
		t, testHandler.RegisterTuituiBYO,
		tuituiBYORequest(userID, agentID, map[string]any{
			"app_id":     tuituiUniqueAppID("tuitui"),
			"app_secret": "pasted-app-secret",
		}),
		http.StatusOK,
	)
	dbfx.Cleanup(t, `DELETE FROM channel_installation WHERE id = $1`, out.ID)
	dbfx.Cleanup(t, `DELETE FROM channel_user_binding WHERE installation_id = $1`, out.ID)
	dbfx.Cleanup(t, `DELETE FROM channel_binding_token WHERE installation_id = $1`, out.ID)
	return out
}

func TestRegisterTuituiBYO_SealsAppSecretAtRest(t *testing.T) {
	box := wireTuituiInstallService(t)
	agentID, ownerID, _ := privateAgentTestFixture(t)
	const appID = "tuitui-seal-check"
	const appSecret = "super-secret-tuitui-app-secret"

	response := testutil.Call(t, testHandler.RegisterTuituiBYO,
		tuituiBYORequest(ownerID, agentID, RegisterTuituiBYORequest{AppID: appID, AppSecret: appSecret}))
	response.Want(http.StatusOK)
	dbfx.Cleanup(t, `DELETE FROM channel_installation WHERE config ->> 'app_id' = $1`, appID)
	if strings.Contains(response.Text(), appSecret) {
		t.Fatalf("install response echoed the secret: %s", response.Text())
	}
	var out TuituiInstallationResponse
	response.JSON(&out)
	if out.WorkspaceID != testWorkspaceID || out.AgentID != agentID || out.InstallerUserID != ownerID {
		t.Fatalf("installation identity = %+v", out)
	}
	if out.ID == "" || out.Status != "active" || out.InstalledAt == "" {
		t.Fatalf("installation fields = %+v", out)
	}

	var storedConfig []byte
	if err := testPool.QueryRow(context.Background(),
		`SELECT config FROM channel_installation WHERE id = $1`, out.ID).Scan(&storedConfig); err != nil {
		t.Fatalf("read stored config: %v", err)
	}
	if strings.Contains(string(storedConfig), appSecret) {
		t.Fatalf("stored config contains the plaintext secret: %s", storedConfig)
	}
	var decoded struct {
		AppID              string `json:"app_id"`
		AppSecret          string `json:"app_secret"`
		AppSecretEncrypted string `json:"app_secret_encrypted"`
	}
	if err := json.Unmarshal(storedConfig, &decoded); err != nil {
		t.Fatalf("decode stored config: %v: %s", err, storedConfig)
	}
	if decoded.AppID != appID {
		t.Fatalf("stored app_id = %q, want %q — inbound routing reads this column", decoded.AppID, appID)
	}
	if decoded.AppSecret != "" {
		t.Fatalf("stored config carries a plaintext app_secret field: %s", storedConfig)
	}
	sealed, err := base64.StdEncoding.DecodeString(decoded.AppSecretEncrypted)
	if err != nil {
		t.Fatalf("stored app_secret_encrypted is not base64: %v", err)
	}
	opened, err := box.Open(sealed)
	if err != nil {
		t.Fatalf("stored ciphertext does not open with the deployment key: %v", err)
	}
	if string(opened) != appSecret {
		t.Fatalf("opened secret = %q, want the pasted secret", opened)
	}
}

func TestRegisterTuituiBYO_RejectsBadRequestsWithoutFiveHundred(t *testing.T) {
	wireTuituiInstallService(t)
	agentID, ownerID, _ := privateAgentTestFixture(t)

	t.Run("malformed json", func(t *testing.T) {
		testutil.Call(t, testHandler.RegisterTuituiBYO,
			tuituiBYORequest(ownerID, agentID, "{not json")).Want(http.StatusBadRequest)
	})
	t.Run("missing agent id", func(t *testing.T) {
		testutil.Call(t, testHandler.RegisterTuituiBYO,
			tuituiBYORequest(ownerID, "", map[string]any{"app_id": "a", "app_secret": "b"})).
			Want(http.StatusBadRequest)
	})
	t.Run("unparseable agent id", func(t *testing.T) {
		testutil.Call(t, testHandler.RegisterTuituiBYO,
			tuituiBYORequest(ownerID, "not-a-uuid", map[string]any{"app_id": "a", "app_secret": "b"})).
			Want(http.StatusBadRequest)
	})
	t.Run("agent outside this workspace", func(t *testing.T) {
		testutil.Call(t, testHandler.RegisterTuituiBYO,
			tuituiBYORequest(ownerID, uuid.NewString(), map[string]any{"app_id": "a", "app_secret": "b"})).
			Want(http.StatusNotFound)
	})
	t.Run("empty app id", func(t *testing.T) {
		testutil.Call(t, testHandler.RegisterTuituiBYO,
			tuituiBYORequest(ownerID, agentID, map[string]any{"app_id": "  ", "app_secret": "b"})).
			Want(http.StatusBadRequest)
	})
	t.Run("empty app secret", func(t *testing.T) {
		testutil.Call(t, testHandler.RegisterTuituiBYO,
			tuituiBYORequest(ownerID, agentID, map[string]any{"app_id": "a", "app_secret": ""})).
			Want(http.StatusBadRequest)
	})
}

func TestRegisterTuituiBYO_FollowsAgentManagePermission(t *testing.T) {
	wireTuituiInstallService(t)
	agentID, ownerID, memberID := privateAgentTestFixture(t)
	adminID := createPermissionTestAdmin(t, "tuitui-register-admin@multica.test")
	payload := map[string]any{"app_id": tuituiUniqueAppID("tuitui-perm"), "app_secret": "perm-secret"}

	// A plain member cannot connect a bot to someone else's agent…
	testutil.Call(t, testHandler.RegisterTuituiBYO,
		tuituiBYORequest(memberID, agentID, payload)).Want(http.StatusForbidden)
	// …while the agent owner and a workspace owner/admin all can. Each of them
	// may be the first to write the row, so the rest legitimately see the
	// already-connected conflict rather than a second success.
	for _, userID := range []string{ownerID, adminID, testUserID} {
		rec := testutil.Call(t, testHandler.RegisterTuituiBYO,
			tuituiBYORequest(userID, agentID, payload))
		rec.WantOneOf(http.StatusOK, http.StatusConflict)
	}
	dbfx.Cleanup(t, `DELETE FROM channel_installation WHERE config ->> 'app_id' = $1`, payload["app_id"])
}

func TestRegisterTuituiBYO_SameAppOnSecondAgentConflicts(t *testing.T) {
	wireTuituiInstallService(t)
	agentID, ownerID, _ := privateAgentTestFixture(t)
	secondAgentID := dbfx.Agent(t, "Tuitui second agent", handlerTestRuntimeID(t),
		testutil.Cols{"owner_id": ownerID})
	appID := tuituiUniqueAppID("tuitui-conflict")
	payload := map[string]any{"app_id": appID, "app_secret": "shared-secret"}
	dbfx.Cleanup(t, `DELETE FROM channel_installation WHERE config ->> 'app_id' = $1`, appID)

	first := testutil.Call(t, testHandler.RegisterTuituiBYO,
		tuituiBYORequest(ownerID, agentID, payload))
	first.Want(http.StatusOK)
	var created TuituiInstallationResponse
	first.JSON(&created)
	// Re-pasting the same bot on the SAME agent updates it in place: the
	// installation id — and with it its bindings — survives.
	reconnect := testutil.Decode[TuituiInstallationResponse](t, testHandler.RegisterTuituiBYO,
		tuituiBYORequest(ownerID, agentID, payload), http.StatusOK)
	if reconnect.ID != created.ID {
		t.Fatalf("reconnect installation = %s, want the existing %s", reconnect.ID, created.ID)
	}
	// A different agent holding the same bot identity is refused: one Tuitui app
	// is one bot, and the (channel_type, app_id) routing index cannot serve two.
	rec := testutil.Call(t, testHandler.RegisterTuituiBYO,
		tuituiBYORequest(ownerID, secondAgentID, payload))
	rec.Want(http.StatusConflict)
	if !strings.Contains(rec.Text(), "another agent in this workspace") {
		t.Fatalf("conflict message = %s", rec.Text())
	}
}

func TestListTuituiInstallations_ConfiguredFalseThenTrue(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	listAs := func(userID string) map[string]any {
		t.Helper()
		return testutil.Call(t, testHandler.ListTuituiInstallations,
			withURLParams(newRequestAs(userID, http.MethodGet,
				"/api/workspaces/"+testWorkspaceID+"/tuitui/installations", nil),
				"id", testWorkspaceID)).Map()
	}

	// The unconfigured answer is a 200 with configured:false, so the panel
	// renders its "ask the operator to enable Tuitui" state instead of an empty
	// list that reads as "nobody has connected a bot".
	previous := testHandler.TuituiInstall
	testHandler.TuituiInstall = nil
	t.Cleanup(func() { testHandler.TuituiInstall = previous })
	unconfigured := listAs(testUserID)
	if unconfigured["configured"] != false {
		t.Fatalf("unconfigured configured = %v, want false", unconfigured["configured"])
	}
	if rows, ok := unconfigured["installations"].([]any); !ok || len(rows) != 0 {
		t.Fatalf("unconfigured installations = %v, want []", unconfigured["installations"])
	}

	agentID, ownerID, _ := privateAgentTestFixture(t)
	wireTuituiInstallService(t)
	out := tuituiInstallByo(t, ownerID, agentID)

	configured := listAs(testUserID)
	if configured["configured"] != true {
		t.Fatalf("configured = %v, want true", configured["configured"])
	}
	rows, ok := configured["installations"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("configured installations = %v, want one row", configured["installations"])
	}
	row, ok := rows[0].(map[string]any)
	if !ok || row["id"] != out.ID || row["status"] != "active" {
		t.Fatalf("listed row = %v, want id %s active", rows[0], out.ID)
	}
	if _, leaked := row["app_secret"]; leaked {
		t.Fatalf("listed row carries an app_secret field: %v", row)
	}
}

func TestListTuituiInstallations_ScopedToTheRequestedWorkspace(t *testing.T) {
	wireTuituiInstallService(t)
	agentID, ownerID, _ := privateAgentTestFixture(t)
	tuituiInstallByo(t, ownerID, agentID)

	// The test fixture's owner account is a member of the second workspace too,
	// so this reaches the listing rather than the membership gate and proves the
	// filter is by workspace, not by "the caller is an admin somewhere".
	otherWorkspaceID := dbfx.Workspace(t, "Tuitui other", "tuitui-other-"+strings.ToLower(uuid.NewString()[:8]))
	dbfx.Member(t, otherWorkspaceID, testUserID, "owner")
	body := testutil.Call(t, testHandler.ListTuituiInstallations,
		withURLParams(newRequestAs(testUserID, http.MethodGet,
			"/api/workspaces/"+otherWorkspaceID+"/tuitui/installations", nil),
			"id", otherWorkspaceID)).Map()
	if rows, ok := body["installations"].([]any); !ok || len(rows) != 0 {
		t.Fatalf("other workspace saw %v", body["installations"])
	}
}

func TestRevokeTuituiInstallation_AuthorizesOwnerAndAdminOnly(t *testing.T) {
	wireTuituiInstallService(t)
	agentID, ownerID, memberID := privateAgentTestFixture(t)
	adminID := createPermissionTestAdmin(t, "tuitui-revoke-admin@multica.test")
	out := tuituiInstallByo(t, ownerID, agentID)

	revoke := func(userID, installationID string) *testutil.Response {
		t.Helper()
		return testutil.Call(t, testHandler.RevokeTuituiInstallation,
			withURLParams(newRequestAs(userID, http.MethodDelete,
				"/api/workspaces/"+testWorkspaceID+"/tuitui/installations/"+installationID, nil),
				"id", testWorkspaceID, "installationId", installationID))
	}

	// A plain member cannot disconnect someone else's bot…
	revoke(memberID, out.ID).Want(http.StatusForbidden)
	// …and a guessed id from another workspace is a 404, not an existence leak.
	revoke(testUserID, uuid.NewString()).Want(http.StatusNotFound)
	// A workspace admin can clean up the installation.
	revoke(adminID, out.ID).Want(http.StatusNoContent)

	var status string
	if err := testPool.QueryRow(context.Background(),
		`SELECT status FROM channel_installation WHERE id = $1`, out.ID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "revoked" {
		t.Fatalf("status after revoke = %q, want revoked", status)
	}
	// Revoking again is idempotent: the row is soft-revoked, never deleted.
	revoke(adminID, out.ID).Want(http.StatusNoContent)
}

func TestTuituiBindingRedeem_MintsBindsAndRefusesReuse(t *testing.T) {
	wireTuituiInstallService(t)
	agentID, ownerID, _ := privateAgentTestFixture(t)
	out := tuituiInstallByo(t, ownerID, agentID)

	token, err := testHandler.TuituiBindingTokens.Mint(
		context.Background(), parseUUID(testWorkspaceID), parseUUID(out.ID), "tuitui-user-1")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	redeem := func(userID, raw string) *testutil.Response {
		t.Helper()
		req := testutil.JSONRequest(http.MethodPost, "/api/tuitui/binding/redeem",
			map[string]any{"token": raw})
		req.Header.Set("X-User-ID", userID)
		return testutil.Call(t, testHandler.RedeemTuituiBindingToken, req)
	}

	// Malformed input and an unknown token are distinct failures: a 400 for the
	// request the caller built, a 410 for a link that can no longer be honoured.
	noToken := testutil.JSONRequest(http.MethodPost, "/api/tuitui/binding/redeem", map[string]any{})
	noToken.Header.Set("X-User-ID", ownerID)
	testutil.Call(t, testHandler.RedeemTuituiBindingToken, noToken).Want(http.StatusBadRequest)
	malformed := testutil.JSONRequest(http.MethodPost, "/api/tuitui/binding/redeem", "{oops")
	malformed.Header.Set("X-User-ID", ownerID)
	testutil.Call(t, testHandler.RedeemTuituiBindingToken, malformed).Want(http.StatusBadRequest)
	redeem(testUserID, "never-minted").Want(http.StatusGone)

	// The workspace owner redeems: their identity is the one the member-visible
	// listing is allowed to echo back (see the canViewAll gate it shares with
	// DingTalk).
	res := redeem(testUserID, token.Raw)
	res.Want(http.StatusOK)
	var redeemed RedeemTuituiBindingTokenResponse
	res.JSON(&redeemed)
	if redeemed.WorkspaceID != testWorkspaceID || redeemed.InstallationID != out.ID ||
		redeemed.TuituiUserID != "tuitui-user-1" {
		t.Fatalf("redeemed identity = %+v", redeemed)
	}
	// Single use: the second click on the same link is refused.
	redeem(testUserID, token.Raw).Want(http.StatusGone)

	// The listing surfaces the member's own identity, which is the only one it
	// may surface.
	list := testutil.Call(t, testHandler.ListTuituiInstallations,
		withURLParams(newRequestAs(testUserID, http.MethodGet,
			"/api/workspaces/"+testWorkspaceID+"/tuitui/installations", nil),
			"id", testWorkspaceID)).Map()
	rows, ok := list["installations"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("installations = %v", list["installations"])
	}
	row := rows[0].(map[string]any)
	bound, ok := row["bound_tuitui_user_ids"].([]any)
	if !ok || len(bound) != 1 || bound[0] != "tuitui-user-1" {
		t.Fatalf("bound_tuitui_user_ids = %v, want [tuitui-user-1]", row["bound_tuitui_user_ids"])
	}
}

func TestTuituiBindingRedeem_RequiresMembershipAndAConfiguredService(t *testing.T) {
	wireTuituiInstallService(t)
	agentID, ownerID, _ := privateAgentTestFixture(t)
	out := tuituiInstallByo(t, ownerID, agentID)
	nonMemberID := dbfx.User(t, "Tuitui Non Member",
		"tuitui-non-member-"+strings.ToLower(uuid.NewString()[:8])+"@multica.test")

	token, err := testHandler.TuituiBindingTokens.Mint(
		context.Background(), parseUUID(testWorkspaceID), parseUUID(out.ID), "tuitui-outsider")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	redeemAs := func(userID, raw string) *testutil.Response {
		t.Helper()
		req := testutil.JSONRequest(http.MethodPost, "/api/tuitui/binding/redeem",
			map[string]any{"token": raw})
		req.Header.Set("X-User-ID", userID)
		return testutil.Call(t, testHandler.RedeemTuituiBindingToken, req)
	}

	// 403 for a non-member — and because the transaction rolls the consume back,
	// the same link still works for someone who IS a member.
	redeemAs(nonMemberID, token.Raw).Want(http.StatusForbidden)
	redeemAs(ownerID, token.Raw).Want(http.StatusOK)

	// With no binding service at all — no master key on this deployment — the
	// endpoint says so instead of pretending a real token is merely wrong.
	previous := testHandler.TuituiBindingTokens
	testHandler.TuituiBindingTokens = nil
	t.Cleanup(func() { testHandler.TuituiBindingTokens = previous })
	redeemAs(ownerID, "whatever").Want(http.StatusForbidden)
}

func TestTuituiGroups_ListAndForgetAndAgentScope(t *testing.T) {
	wireTuituiInstallService(t)
	agentID, ownerID, memberID := privateAgentTestFixture(t)
	out := tuituiInstallByo(t, ownerID, agentID)

	const conversationID = "teams_eng_release_thread-9"
	sessionID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "Release room"})
	groupBindingID := dbfx.Insert(t, "channel_chat_session_binding", testutil.Cols{
		"chat_session_id": sessionID,
		"installation_id": out.ID,
		"channel_type":    "tuitui",
		"channel_chat_id": conversationID,
		"chat_type":       "group",
	})
	dbfx.Insert(t, "channel_chat_session_binding", testutil.Cols{
		"chat_session_id": dbfx.ChatSession(t, agentID, testutil.Cols{"title": "Direct message"}),
		"installation_id": out.ID,
		"channel_type":    "tuitui",
		"channel_chat_id": "10086",
		"chat_type":       "p2p",
	})

	listWorkspace := func(userID string) ListTuituiGroupsResponse {
		t.Helper()
		return testutil.Decode[ListTuituiGroupsResponse](t, testHandler.ListTuituiGroups,
			withURLParams(newRequestAs(userID, http.MethodGet,
				"/api/workspaces/"+testWorkspaceID+"/tuitui/groups", nil),
				"id", testWorkspaceID),
			http.StatusOK)
	}
	got := listWorkspace(testUserID)
	if len(got.Groups) != 1 {
		t.Fatalf("groups = %+v, want only the group session (p2p stays out)", got.Groups)
	}
	group := got.Groups[0]
	if group.ConversationID != conversationID || group.ConversationTitle != "Release room" {
		t.Fatalf("group = %+v", group)
	}
	if len(group.Bots) != 1 || group.Bots[0].AgentID != agentID ||
		group.Bots[0].InstallationID != out.ID || group.Bots[0].LastActiveAt == "" {
		t.Fatalf("bots = %+v", group.Bots)
	}
	if !got.GroupDiscoverySupported {
		t.Fatal("group_discovery_supported = false, want true for a build that serves the route")
	}

	// The agent-scoped route reuses the Agent view gate and reports the same row.
	agentView := testutil.Decode[ListTuituiGroupsResponse](t, testHandler.ListTuituiGroupsForAgent,
		withURLParams(newRequestAs(ownerID, http.MethodGet,
			"/api/agents/"+agentID+"/tuitui/groups", nil), "id", agentID),
		http.StatusOK)
	if len(agentView.Groups) != 1 || agentView.Groups[0].ConversationID != conversationID {
		t.Fatalf("agent groups = %+v", agentView.Groups)
	}

	// Forget is owner/admin-only, and a conversation that is not live is a 404.
	forget := func(userID, conversationID string) *testutil.Response {
		t.Helper()
		return testutil.Call(t, testHandler.ForgetTuituiGroup,
			withURLParams(newRequestAs(userID, http.MethodDelete,
				"/api/workspaces/"+testWorkspaceID+"/tuitui/installations/"+out.ID+
					"/groups/"+url.PathEscape(conversationID), nil),
				"id", testWorkspaceID, "installationId", out.ID, "conversationId", conversationID))
	}
	forget(memberID, conversationID).Want(http.StatusForbidden)
	forget(testUserID, "teams_eng_nope").Want(http.StatusNotFound)
	forget(testUserID, conversationID).Want(http.StatusNoContent)

	if after := listWorkspace(testUserID); len(after.Groups) != 0 {
		t.Fatalf("groups after forget = %+v, want empty", after.Groups)
	}
	// Forgetting retires the route only: the chat, its history, and the bot stay.
	var sessionCount int
	var retired bool
	var installationCount int
	if err := testPool.QueryRow(context.Background(), `
SELECT count(*) FROM chat_session WHERE id = $1
`, sessionID).Scan(&sessionCount); err != nil {
		t.Fatalf("read chat session: %v", err)
	}
	if err := testPool.QueryRow(context.Background(),
		`SELECT retired_at IS NOT NULL FROM channel_chat_session_binding WHERE id = $1`,
		groupBindingID).Scan(&retired); err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM channel_installation WHERE id = $1`, out.ID).Scan(&installationCount); err != nil {
		t.Fatalf("read installation: %v", err)
	}
	if sessionCount != 1 || !retired || installationCount != 1 {
		t.Fatalf("post-forget state = session %d retired %v installation %d",
			sessionCount, retired, installationCount)
	}
}

func TestTuituiGroups_InvalidActivityFilterAndUnconfiguredService(t *testing.T) {
	wireTuituiInstallService(t)
	groups := func(query string) *testutil.Response {
		t.Helper()
		return testutil.Call(t, testHandler.ListTuituiGroups,
			withURLParams(newRequestAs(testUserID, http.MethodGet,
				"/api/workspaces/"+testWorkspaceID+"/tuitui/groups"+query, nil),
				"id", testWorkspaceID))
	}
	groups("?activity=recent").Want(http.StatusBadRequest)
	groups("?activity=inactive").Want(http.StatusBadRequest)

	// Inactive paging is accepted but always empty: Tuitui keeps no presence
	// history, so there is no out-of-window set and no cursor to follow. The
	// optional paging fields stay absent rather than sent as zero values, which
	// is what tells a client this page is complete.
	inactive := testutil.Call(t, testHandler.ListTuituiGroups,
		func() *http.Request {
			return withURLParams(newRequestAs(testUserID, http.MethodGet,
				"/api/workspaces/"+testWorkspaceID+
					"/tuitui/groups?activity=inactive&installation_id="+uuid.NewString(), nil),
				"id", testWorkspaceID)
		}()).Want(http.StatusOK).Map()
	if rows, ok := inactive["groups"].([]any); !ok || len(rows) != 0 {
		t.Fatalf("inactive page = %v, want empty groups", inactive["groups"])
	}
	for _, absent := range []string{"next_offset", "inactive_group_counts", "bot_identities"} {
		if _, found := inactive[absent]; found {
			t.Fatalf("inactive page carries %s = %v, want it omitted", absent, inactive[absent])
		}
	}

	// With no master key this deployment serves none of the write paths, and the
	// inventory answers as empty rather than erroring.
	previous := testHandler.TuituiInstall
	testHandler.TuituiInstall = nil
	t.Cleanup(func() { testHandler.TuituiInstall = previous })
	empty := testutil.Decode[ListTuituiGroupsResponse](t, testHandler.ListTuituiGroups,
		withURLParams(newRequestAs(testUserID, http.MethodGet,
			"/api/workspaces/"+testWorkspaceID+"/tuitui/groups", nil), "id", testWorkspaceID),
		http.StatusOK)
	if len(empty.Groups) != 0 || !empty.GroupDiscoverySupported {
		t.Fatalf("unconfigured groups answer = %+v", empty)
	}
	testutil.Call(t, testHandler.RevokeTuituiInstallation,
		withURLParams(newRequestAs(testUserID, http.MethodDelete,
			"/api/workspaces/"+testWorkspaceID+"/tuitui/installations/"+uuid.NewString(), nil),
			"id", testWorkspaceID, "installationId", uuid.NewString())).Want(http.StatusForbidden)
	testutil.Call(t, testHandler.RegisterTuituiBYO,
		tuituiBYORequest(testUserID, uuid.NewString(),
			map[string]any{"app_id": "a", "app_secret": "b"})).
		Want(http.StatusForbidden)
}

// TestGetConfig_TuituiSupported keeps the fail-closed contract clients depend
// on: this build declares the Tuitui routes as a real JSON boolean, not a field
// that only exists in the Go struct.
func TestGetConfig_TuituiSupported(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	rec := testutil.Call(t, testHandler.GetConfig,
		newRequest(http.MethodGet, "/api/config", nil)).Want(http.StatusOK)
	var cfg AppConfig
	rec.JSON(&cfg)
	if !cfg.TuituiSupported {
		t.Fatal("tuitui_supported = false, want true")
	}
	raw := map[string]any{}
	if err := json.Unmarshal([]byte(rec.Text()), &raw); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if raw["tuitui_supported"] != true {
		t.Fatalf("tuitui_supported = %v, want true", raw["tuitui_supported"])
	}
}
