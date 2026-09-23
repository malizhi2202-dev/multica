package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	tuituiintegration "github.com/multica-ai/multica/server/internal/integrations/tuitui"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// tuitui_allowlist_test.go covers the two surfaces the allowlist panel talks to:
// the narrow write of one bot's inbound gates, and the read-only list of
// subjects that write recently turned away. The decision itself is tested where
// it is made (integrations/tuitui/allow_test.go); what lives here is the shape
// stored for the operator's words, the echo the dialog re-renders from it, and
// who may see or change either.

// createTuituiNonMember makes a user who belongs to no workspace, for the gates
// that keep a stranger out of (and away from) the allowlist.
func createTuituiNonMember(t *testing.T, email string) string {
	t.Helper()
	var userID string
	if err := testPool.QueryRow(context.Background(), `
INSERT INTO "user" (name, email) VALUES ('Tuitui non-member', $1) RETURNING id
`, email).Scan(&userID); err != nil {
		t.Fatalf("create Tuitui non-member: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userID)
	})
	return userID
}

func tuituiStrPtr(s string) *string { return &s }

// tuituiStoredConfig reads one installation's stored config blob as raw JSON
// values, so a test can assert the SHAPE of what was written — key absent versus
// key present and empty is the whole feature, and the echo cannot prove it.
func tuituiStoredConfig(t *testing.T, installationID string) map[string]json.RawMessage {
	t.Helper()
	raw := tuituiStoredConfigBytes(t, installationID)
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode stored config %s: %v", raw, err)
	}
	return decoded
}

func tuituiStoredConfigBytes(t *testing.T, installationID string) []byte {
	t.Helper()
	var raw []byte
	if err := testPool.QueryRow(context.Background(),
		`SELECT config FROM channel_installation WHERE id = $1`, installationID).Scan(&raw); err != nil {
		t.Fatalf("read stored config: %v", err)
	}
	return raw
}

func storedConfigKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func manyAccounts(n int) string {
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		parts = append(parts, fmt.Sprintf("user-%d", i))
	}
	return strings.Join(parts, ",")
}

func tuituiAllowlistPutRequest(userID, workspaceID, installationID string, body any) *http.Request {
	path := "/api/workspaces/" + workspaceID + "/tuitui/installations/" + installationID + "/allowlists"
	return withURLParams(newRequestAs(userID, http.MethodPut, path, body),
		"id", workspaceID, "installationId", installationID)
}

// tuituiPutAllowlists saves one bot's gates and returns the echoed installation.
func tuituiPutAllowlists(t *testing.T, userID string, inst TuituiInstallationResponse, body UpdateTuituiAllowlistsRequest) TuituiInstallationResponse {
	t.Helper()
	return testutil.Decode[TuituiInstallationResponse](t, testHandler.UpdateTuituiAllowlists,
		tuituiAllowlistPutRequest(userID, inst.WorkspaceID, inst.ID, body), http.StatusOK)
}

// tuituiInstallByoWith builds a bot whose gate is a real list: a refusal is only
// worth showing while the subject is still outside it, so a test that wants a
// pending refusal has to start from a restricted installation.
func tuituiInstallByoWith(t *testing.T, userID, agentID, direct, group string) TuituiInstallationResponse {
	t.Helper()
	inst := tuituiInstallByo(t, userID, agentID)
	return tuituiPutAllowlists(t, userID, inst, UpdateTuituiAllowlistsRequest{
		AllowFrom:      tuituiStrPtr(direct),
		GroupAllowFrom: tuituiStrPtr(group),
	})
}

func TestUpdateTuituiAllowlists_StoredShapeAndEchoAgree(t *testing.T) {
	wireTuituiInstallService(t)
	agentID, ownerID, _ := privateAgentTestFixture(t)

	cases := []struct {
		name   string
		direct *string
		group  *string
		// wantStored is the decoded entry list per key; wantPresent says whether
		// the key must exist at all. The two together are the tri-state, and the
		// panel can only render "not configured" apart from "nobody" if the row
		// keeps them apart.
		wantDirectPresent bool
		wantDirect        []string
		wantGroupPresent  bool
		wantGroup         []string
		wantDirectMode    string
		wantGroupMode     string
		// allowsStranger is what the stored gate really does with an account
		// nobody named: the behaviour the operator is buying.
		allowsStranger bool
	}{
		{
			name:              "wildcard is unrestricted",
			direct:            tuituiStrPtr("*"),
			group:             tuituiStrPtr("*"),
			wantDirectPresent: true,
			wantDirect:        []string{"*"},
			wantGroupPresent:  true,
			wantGroup:         []string{"*"},
			wantDirectMode:    "unrestricted",
			wantGroupMode:     "unrestricted",
			allowsStranger:    true,
		},
		{
			name:              "typed ids are trimmed and de-duplicated",
			direct:            tuituiStrPtr(" alice , bob ,alice "),
			group:             tuituiStrPtr("1234567890123456, teams_t1_c2"),
			wantDirectPresent: true,
			wantDirect:        []string{"alice", "bob"},
			wantGroupPresent:  true,
			wantGroup:         []string{"1234567890123456", "teams_t1_c2"},
			wantDirectMode:    "allowlist",
			wantGroupMode:     "allowlist",
			allowsStranger:    false,
		},
		{
			// The distinction the dialog warns about: clearing the box stores []
			// (nobody), not an absent key (nobody restricted).
			name:              "cleared fields deny everyone",
			direct:            tuituiStrPtr(""),
			group:             tuituiStrPtr("   "),
			wantDirectPresent: true,
			wantDirect:        []string{},
			wantGroupPresent:  true,
			wantGroup:         []string{},
			wantDirectMode:    "allowlist",
			wantGroupMode:     "allowlist",
			allowsStranger:    false,
		},
		{
			// The reference tests the wildcard for membership before any id, so
			// ids beside it do nothing and are not kept as a fake restriction.
			name:              "wildcard beside ids is stored as the wildcard alone",
			direct:            tuituiStrPtr("alice,*"),
			group:             tuituiStrPtr("*"),
			wantDirectPresent: true,
			wantDirect:        []string{"*"},
			wantGroupPresent:  true,
			wantGroup:         []string{"*"},
			wantDirectMode:    "unrestricted",
			wantGroupMode:     "unrestricted",
			allowsStranger:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inst := tuituiInstallByo(t, ownerID, agentID)

			out := tuituiPutAllowlists(t, ownerID, inst, UpdateTuituiAllowlistsRequest{
				AllowFrom:      tc.direct,
				GroupAllowFrom: tc.group,
			})

			// 1. What landed in the row, compared as decoded JSON: jsonb
			// re-spaces its own output, and presence and order are what is under
			// test here.
			stored := tuituiStoredConfig(t, inst.ID)
			for key, want := range map[string]struct {
				present bool
				entries []string
			}{
				"allow_from":       {tc.wantDirectPresent, tc.wantDirect},
				"group_allow_from": {tc.wantGroupPresent, tc.wantGroup},
			} {
				value, present := stored[key]
				if !want.present {
					if present {
						t.Fatalf("%s must not be stored at all, got %s", key, value)
					}
					continue
				}
				if !present {
					t.Fatalf("%s must be stored as %v, stored keys: %v", key, want.entries, storedConfigKeys(stored))
				}
				var entries []string
				if err := json.Unmarshal(value, &entries); err != nil {
					t.Fatalf("stored %s is not a list of ids: %s", key, value)
				}
				if strings.Join(entries, "|") != strings.Join(want.entries, "|") {
					t.Errorf("stored %s = %v, want %v", key, entries, want.entries)
				}
			}

			// 2. What the endpoint echoes, which is what the dialog re-renders.
			if out.AllowFromMode != tc.wantDirectMode || out.GroupAllowFromMode != tc.wantGroupMode {
				t.Errorf("modes = %q / %q, want %q / %q",
					out.AllowFromMode, out.GroupAllowFromMode, tc.wantDirectMode, tc.wantGroupMode)
			}
			if out.AllowFrom == nil || out.GroupAllowFrom == nil {
				t.Errorf("allowlist fields must serialize as [], not null: %#v", out)
			}
			// An unset gate echoes no ids, so it can never read as "restricted to
			// nothing" on a bot that is in fact open.
			if tc.wantDirectMode == "unrestricted" && len(out.AllowFrom) != 0 {
				t.Errorf("unrestricted direct gate echoed ids: %v", out.AllowFrom)
			}
			if tc.wantGroupMode == "unrestricted" && len(out.GroupAllowFrom) != 0 {
				t.Errorf("unrestricted group gate echoed ids: %v", out.GroupAllowFrom)
			}
			if tc.wantDirectPresent && tc.wantDirectMode == "allowlist" &&
				strings.Join(out.AllowFrom, "|") != strings.Join(tc.wantDirect, "|") {
				t.Errorf("echoed allow_from = %v, want %v", out.AllowFrom, tc.wantDirect)
			}

			// 3. Row, echo, and the gate the connection enforces have to be one
			// story: the connection decodes the same bytes through the same
			// helper, so this is the cross-check rather than a repeat of step 1.
			direct, group := tuituiintegration.AllowlistsFromStoredConfig(tuituiStoredConfigBytes(t, inst.ID))
			if got := direct.Allows("a-stranger"); got != tc.allowsStranger {
				t.Errorf("stored gate allows a stranger = %v, want %v", got, tc.allowsStranger)
			}
			// "teams_unnamed_chan" appears in no case's list, so it is the
			// group-side twin of the stranger above.
			if tc.wantGroupPresent {
				if got := group.Allows("teams_unnamed_chan"); got != tc.allowsStranger {
					t.Errorf("stored group gate allows an unnamed channel = %v, want %v", got, tc.allowsStranger)
				}
			}
		})
	}
}

// TestUpdateTuituiAllowlists_LeavesEveryOtherConfigKeyAlone is why this write is
// one SQL merge rather than a Go read-modify-write: the same blob holds the
// sealed app_secret, and a struct that does not declare a key would drop it on
// the way back out. That is not cosmetic — it makes an existing bot's credential
// undecryptable, on the next restart, far from the click that did it. So the
// comparison here is byte for byte, not "the key is still there".
func TestUpdateTuituiAllowlists_LeavesEveryOtherConfigKeyAlone(t *testing.T) {
	wireTuituiInstallService(t)
	agentID, ownerID, _ := privateAgentTestFixture(t)
	const appSecret = "super-secret-tuitui-app-secret"
	appID := tuituiUniqueAppID("tuitui-untouched")
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(),
			`DELETE FROM channel_installation WHERE config ->> 'app_id' = $1`, appID)
	})
	inst := testutil.Decode[TuituiInstallationResponse](t, testHandler.RegisterTuituiBYO,
		tuituiBYORequest(ownerID, agentID, RegisterTuituiBYORequest{
			BaseURL: "https://tuitui.test:8282", AppID: appID, AppSecret: appSecret,
		}), http.StatusOK)

	before := tuituiStoredConfig(t, inst.ID)
	out := tuituiPutAllowlists(t, ownerID, inst, UpdateTuituiAllowlistsRequest{
		AllowFrom:      tuituiStrPtr("alice"),
		GroupAllowFrom: tuituiStrPtr("778899"),
	})
	after := tuituiStoredConfig(t, inst.ID)

	if string(before["app_secret_encrypted"]) != string(after["app_secret_encrypted"]) {
		t.Errorf("app_secret_encrypted changed:\n before %s\n after  %s",
			before["app_secret_encrypted"], after["app_secret_encrypted"])
	}
	var sealed string
	if err := json.Unmarshal(after["app_secret_encrypted"], &sealed); err != nil {
		t.Fatalf("app_secret_encrypted is not a JSON string: %s", after["app_secret_encrypted"])
	}
	if sealed == "" || strings.Contains(sealed, appSecret) {
		t.Errorf("the sealed credential must survive intact and never in plaintext, got %q", sealed)
	}
	for _, key := range []string{"app_id", "host", "port"} {
		if string(before[key]) != string(after[key]) {
			t.Errorf("%s changed: before %s, after %s", key, before[key], after[key])
		}
	}
	// The gate itself landed, and the echo repeats it rather than the input.
	if strings.Join(out.AllowFrom, "|") != "alice" || strings.Join(out.GroupAllowFrom, "|") != "778899" {
		t.Errorf("echo = %v / %v", out.AllowFrom, out.GroupAllowFrom)
	}
	// A management response must not carry the secret in either spelling.
	if body := testutil.Call(t, testHandler.UpdateTuituiAllowlists,
		tuituiAllowlistPutRequest(ownerID, testWorkspaceID, inst.ID, UpdateTuituiAllowlistsRequest{
			AllowFrom: tuituiStrPtr("alice,bob"),
		})).Text(); strings.Contains(body, appSecret) || strings.Contains(body, "app_secret") {
		t.Errorf("the allowlist response leaked a secret field: %s", body)
	}
}

// TestUpdateTuituiAllowlists_OmittedFieldLeavesThatListAlone pins the only
// partial-write semantics this endpoint has: saving one box must not silently
// clear the other.
func TestUpdateTuituiAllowlists_OmittedFieldLeavesThatListAlone(t *testing.T) {
	wireTuituiInstallService(t)
	agentID, ownerID, _ := privateAgentTestFixture(t)
	inst := tuituiInstallByoWith(t, ownerID, agentID, "alice", "778899")

	// A body with neither key is a no-op on both lists, not a reset.
	untouched := testutil.Decode[TuituiInstallationResponse](t, testHandler.UpdateTuituiAllowlists,
		tuituiAllowlistPutRequest(ownerID, testWorkspaceID, inst.ID, UpdateTuituiAllowlistsRequest{}),
		http.StatusOK)
	if strings.Join(untouched.AllowFrom, "|") != "alice" ||
		strings.Join(untouched.GroupAllowFrom, "|") != "778899" {
		t.Fatalf("an empty body changed the gates: %+v", untouched)
	}

	// Tightening the group list leaves the personal list exactly as it was.
	after := tuituiPutAllowlists(t, ownerID, inst, UpdateTuituiAllowlistsRequest{
		GroupAllowFrom: tuituiStrPtr("1234567890123456"),
	})
	if strings.Join(after.AllowFrom, "|") != "alice" {
		t.Errorf("allow_from = %v, want the untouched alice", after.AllowFrom)
	}
	if strings.Join(after.GroupAllowFrom, "|") != "1234567890123456" {
		t.Errorf("group_allow_from = %v, want the new id", after.GroupAllowFrom)
	}
	if got := string(tuituiStoredConfig(t, inst.ID)["allow_from"]); got != `["alice"]` {
		t.Errorf("stored allow_from = %s, want it carried over unchanged", got)
	}

	// An explicit empty string is the opposite of an omitted key: it clears.
	cleared := tuituiPutAllowlists(t, ownerID, inst, UpdateTuituiAllowlistsRequest{
		GroupAllowFrom: tuituiStrPtr(""),
	})
	if len(cleared.GroupAllowFrom) != 0 || cleared.GroupAllowFromMode != "allowlist" {
		t.Fatalf("cleared group gate = %v / %q, want [] and allowlist",
			cleared.GroupAllowFrom, cleared.GroupAllowFromMode)
	}
	if got := string(tuituiStoredConfig(t, inst.ID)["group_allow_from"]); got != `[]` {
		t.Errorf("stored group_allow_from = %s, want the empty array that denies everyone", got)
	}
}

func TestUpdateTuituiAllowlists_RejectsMalformedEntries(t *testing.T) {
	wireTuituiInstallService(t)
	agentID, ownerID, _ := privateAgentTestFixture(t)
	inst := tuituiInstallByoWith(t, ownerID, agentID, "alice", "778899")
	before := tuituiStoredConfigBytes(t, inst.ID)

	cases := []struct {
		name   string
		direct *string
		group  *string
	}{
		{
			name:   "entry longer than the stored list can hold",
			direct: tuituiStrPtr("alice," + strings.Repeat("x", tuituiintegration.MaxAllowlistEntryLen+1)),
		},
		{
			name:  "group id with a newline in it",
			group: tuituiStrPtr("1234567890123456\n778899"),
		},
		{
			name:   "more entries than the stored list allows",
			direct: tuituiStrPtr(manyAccounts(tuituiintegration.MaxAllowlistEntries + 1)),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := testutil.Call(t, testHandler.UpdateTuituiAllowlists,
				tuituiAllowlistPutRequest(ownerID, testWorkspaceID, inst.ID, UpdateTuituiAllowlistsRequest{
					AllowFrom:      tc.direct,
					GroupAllowFrom: tc.group,
				}))
			response.Want(http.StatusBadRequest)
			// The 400 has to name which box broke: "invalid request" is not
			// something a dialog can point an input at.
			which := "allow_from"
			if tc.direct == nil {
				which = "group_allow_from"
			}
			if body := response.Text(); !strings.Contains(body, which) {
				t.Errorf("400 body = %s, want it to name %s", body, which)
			}
			// And the gate must still be the one it was. A half-applied edit is
			// the worst outcome here: the operator believes a list is closed and
			// it is not, or the other way round.
			if after := tuituiStoredConfigBytes(t, inst.ID); string(after) != string(before) {
				t.Errorf("a rejected write changed the stored config:\n before %s\n after  %s", before, after)
			}
		})
	}
}

func TestUpdateTuituiAllowlists_AuthorizationAndAvailability(t *testing.T) {
	wireTuituiInstallService(t)
	agentID, ownerID, memberID := privateAgentTestFixture(t)
	inst := tuituiInstallByoWith(t, ownerID, agentID, "alice", "778899")

	put := func(userID, workspaceID, installationID string) *testutil.Response {
		t.Helper()
		return testutil.Call(t, testHandler.UpdateTuituiAllowlists,
			tuituiAllowlistPutRequest(userID, workspaceID, installationID, UpdateTuituiAllowlistsRequest{
				AllowFrom: tuituiStrPtr("bob"),
			}))
	}

	// The agent's own manager may tighten its bot; an unrelated member may not,
	// and the denial says so rather than pretending the row is missing.
	put(ownerID, testWorkspaceID, inst.ID).Want(http.StatusOK)
	put(memberID, testWorkspaceID, inst.ID).Want(http.StatusForbidden)

	// Another workspace's installation is a 404: the same answer as a row that
	// does not exist, so ids cannot be probed across the boundary. The caller owns
	// that workspace, so this is the lookup refusing, not the membership gate.
	otherWorkspaceID := dbfx.Workspace(t, "Tuitui allowlists other", "tuitui-allo-"+strings.ToLower(uuid.NewString()[:8]))
	dbfx.Member(t, otherWorkspaceID, ownerID, "owner")
	put(ownerID, otherWorkspaceID, inst.ID).Want(http.StatusNotFound)

	// A workspace the caller does not belong to never reaches the gate logic.
	outsider := createTuituiNonMember(t, "tuitui-allowlist-non-member@multica.test")
	put(outsider, testWorkspaceID, inst.ID).Want(http.StatusNotFound)

	// A revoked bot is not supervised, so its gate decides nothing: the write is
	// refused rather than parked where nobody will honour it.
	testutil.Call(t, testHandler.RevokeTuituiInstallation,
		withURLParams(newRequestAs(ownerID, http.MethodDelete,
			"/api/workspaces/"+testWorkspaceID+"/tuitui/installations/"+inst.ID, nil),
			"id", testWorkspaceID, "installationId", inst.ID)).Want(http.StatusNoContent)
	put(ownerID, testWorkspaceID, inst.ID).Want(http.StatusConflict)

	// Without a master key the channel cannot run at all, which is the same 403
	// with a machine-readable code every other Tuitui management route gives.
	previous := testHandler.TuituiInstall
	testHandler.TuituiInstall = nil
	t.Cleanup(func() { testHandler.TuituiInstall = previous })
	revoked := put(ownerID, testWorkspaceID, inst.ID)
	revoked.Want(http.StatusForbidden)
	if body := revoked.Text(); !strings.Contains(body, "tuitui_not_configured") {
		t.Errorf("unconfigured response = %s, want the tuitui_not_configured code", body)
	}
}

func TestUpdateTuituiAllowlists_InvalidIdentifiersAndBody(t *testing.T) {
	wireTuituiInstallService(t)
	agentID, ownerID, _ := privateAgentTestFixture(t)
	inst := tuituiInstallByo(t, ownerID, agentID)

	body := UpdateTuituiAllowlistsRequest{AllowFrom: tuituiStrPtr("alice")}
	put := func(workspaceID, installationID string) *testutil.Response {
		t.Helper()
		return testutil.Call(t, testHandler.UpdateTuituiAllowlists,
			tuituiAllowlistPutRequest(ownerID, workspaceID, installationID, body))
	}
	put("not-a-uuid", inst.ID).Want(http.StatusBadRequest)
	put(testWorkspaceID, "not-a-uuid").Want(http.StatusBadRequest)
	// A well-formed id that simply is not a Tuitui installation in this workspace
	// is the same 404 as a missing one.
	put(testWorkspaceID, uuid.NewString()).Want(http.StatusNotFound)
	// A field of the wrong JSON type is the caller's 400, never a 500 and never a
	// silently ignored list: a number cannot be an allowlist.
	testutil.Call(t, testHandler.UpdateTuituiAllowlists,
		tuituiAllowlistPutRequest(ownerID, testWorkspaceID, inst.ID,
			map[string]any{"allow_from": 7})).Want(http.StatusBadRequest)
}

// seedTuituiDrop writes one non-content drop audit row, the only way a refused
// subject becomes visible to the picker.
func seedTuituiDrop(t *testing.T, installationID, chatID, eventType, reason, receivedAt string) {
	t.Helper()
	dbfx.Insert(t, "channel_inbound_audit", testutil.Cols{
		"installation_id": installationID,
		"channel_type":    "tuitui",
		"channel_chat_id": chatID,
		"event_type":      eventType,
		"drop_reason":     reason,
		"received_at":     receivedAt,
	})
	dbfx.Cleanup(t, `
DELETE FROM channel_inbound_audit
WHERE installation_id = $1 AND channel_chat_id IS NOT DISTINCT FROM $2
  AND event_type = $3 AND drop_reason = $4 AND received_at = $5::timestamptz`,
		installationID, chatID, eventType, reason, receivedAt)
}

func seedTuituiRejection(t *testing.T, installationID, chatID, eventType, receivedAt string) {
	t.Helper()
	seedTuituiDrop(t, installationID, chatID, eventType, tuituiintegration.AllowlistReason, receivedAt)
}

func tuituiRejectionsAs(t *testing.T, userID, query string) *testutil.Response {
	t.Helper()
	return testutil.Call(t, testHandler.ListTuituiRejections,
		withURLParams(newRequestAs(userID, http.MethodGet,
			"/api/workspaces/"+testWorkspaceID+"/tuitui/rejections"+query, nil),
			"id", testWorkspaceID))
}

func TestListTuituiRejections_ListsRefusedSubjects(t *testing.T) {
	wireTuituiInstallService(t)
	agentID, ownerID, memberID := privateAgentTestFixture(t)
	// Two gated bots, on two agents: the list is scoped both by workspace and by
	// the one bot a dialog is editing.
	bot := tuituiInstallByoWith(t, ownerID, agentID, "alice", "778899")
	otherAgentID := dbfx.Agent(t, "tuitui-rejection-second-agent", handlerTestRuntimeID(t),
		testutil.Cols{"owner_id": ownerID})
	otherBot := tuituiInstallByoWith(t, ownerID, otherAgentID, "carol", "778899")

	// Pending: bob twice on the first bot, a teams channel once, dora once on the
	// second bot.
	seedTuituiRejection(t, bot.ID, "bob", "single_chat", "2026-01-06 10:00:00+00")
	seedTuituiRejection(t, bot.ID, "bob", "single_chat", "2026-01-05 10:00:00+00")
	seedTuituiRejection(t, bot.ID, "teams_t1_c2", "teams_post_create", "2026-01-03 10:00:00+00")
	seedTuituiRejection(t, otherBot.ID, "dora", "single_chat", "2026-01-04 10:00:00+00")
	// Not pending, each for its own reason: alice and 778899 are on the lists
	// that refused them, dave fell to a different gate, and the last row names no
	// subject at all.
	seedTuituiRejection(t, bot.ID, "alice", "single_chat", "2026-01-07 10:00:00+00")
	seedTuituiRejection(t, bot.ID, "778899", "group_chat", "2026-01-07 11:00:00+00")
	seedTuituiDrop(t, bot.ID, "dave", "single_chat", string(engine.DropReasonUnboundUser), "2026-01-08 10:00:00+00")
	dbfx.Insert(t, "channel_inbound_audit", testutil.Cols{
		"installation_id": bot.ID,
		"channel_type":    "tuitui",
		"event_type":      "group_chat",
		"drop_reason":     tuituiintegration.AllowlistReason,
		"received_at":     "2026-01-09 10:00:00+00",
	})
	dbfx.Cleanup(t, `
DELETE FROM channel_inbound_audit
WHERE installation_id = $1 AND channel_chat_id IS NULL AND received_at = $2::timestamptz`,
		bot.ID, "2026-01-09 10:00:00+00")

	page := testutil.Decode[ListTuituiRejectionsResponse](t, testHandler.ListTuituiRejections,
		withURLParams(newRequestAs(testUserID, http.MethodGet,
			"/api/workspaces/"+testWorkspaceID+"/tuitui/rejections", nil),
			"id", testWorkspaceID),
		http.StatusOK)

	if len(page.Rejections) != 3 {
		t.Fatalf("rejections = %+v, want the three subjects still outside every list", page.Rejections)
	}
	if page.NextOffset != nil {
		t.Errorf("next_offset = %v, want none for an incomplete page", *page.NextOffset)
	}
	got := map[string]TuituiRejectionResponse{}
	for _, row := range page.Rejections {
		got[row.ID] = row
	}
	for _, absent := range []string{"alice", "778899", "dave", ""} {
		if _, ok := got[absent]; ok {
			t.Errorf("subject %q must not be offered: %#v", absent, page.Rejections)
		}
	}
	if row, ok := got["bob"]; !ok {
		t.Error("bob missing")
	} else {
		if row.Kind != "user" {
			t.Errorf("bob kind = %q, want user", row.Kind)
		}
		if row.Count != 2 {
			t.Errorf("bob count = %d, want the two refused messages", row.Count)
		}
		if row.InstallationID != bot.ID || row.AgentID != agentID {
			t.Errorf("bob attribution = %s / %s, want %s / %s",
				row.InstallationID, row.AgentID, bot.ID, agentID)
		}
		if !strings.HasSuffix(row.LastSeenAt, "Z") || row.LastSeenAt == "" {
			t.Errorf("bob last_seen_at = %q, want an RFC3339 instant", row.LastSeenAt)
		}
	}
	if row, ok := got["teams_t1_c2"]; !ok {
		t.Error("the teams channel missing")
	} else if row.Kind != "group" {
		t.Errorf("teams kind = %q, want group so the id lands in the group box", row.Kind)
	}
	// Newest first, so the thing the operator just tried is on top.
	if page.Rejections[0].ID != "bob" {
		t.Errorf("first row = %q, want the most recent refusal", page.Rejections[0].ID)
	}

	// One bot's dialog shows that bot's refusals, not the workspace's.
	filtered := testutil.Decode[ListTuituiRejectionsResponse](t, testHandler.ListTuituiRejections,
		withURLParams(newRequestAs(testUserID, http.MethodGet,
			"/api/workspaces/"+testWorkspaceID+"/tuitui/rejections?installation_id="+bot.ID, nil),
			"id", testWorkspaceID),
		http.StatusOK)
	if len(filtered.Rejections) != 2 {
		t.Fatalf("filtered rejections = %+v, want this bot's two subjects", filtered.Rejections)
	}
	for _, row := range filtered.Rejections {
		if row.InstallationID != bot.ID {
			t.Errorf("filter leaked %s", row.InstallationID)
		}
	}
	// An id from nowhere (or from another workspace) is an empty page, not an
	// existence oracle and not a 500.
	unknown := testutil.Decode[ListTuituiRejectionsResponse](t, testHandler.ListTuituiRejections,
		withURLParams(newRequestAs(testUserID, http.MethodGet,
			"/api/workspaces/"+testWorkspaceID+"/tuitui/rejections?installation_id="+uuid.NewString(), nil),
			"id", testWorkspaceID),
		http.StatusOK)
	if len(unknown.Rejections) != 0 {
		t.Fatalf("unknown installation rejections = %+v", unknown.Rejections)
	}

	// Paging: a full page hands back the offset to continue at, and one past the
	// end is the only answer that can say there is nothing more.
	first := testutil.Decode[ListTuituiRejectionsResponse](t, testHandler.ListTuituiRejections,
		withURLParams(newRequestAs(testUserID, http.MethodGet,
			"/api/workspaces/"+testWorkspaceID+"/tuitui/rejections?limit=2", nil),
			"id", testWorkspaceID),
		http.StatusOK)
	if len(first.Rejections) != 2 || first.NextOffset == nil || *first.NextOffset != 2 {
		t.Fatalf("first page = %+v next=%v", first.Rejections, first.NextOffset)
	}
	second := testutil.Decode[ListTuituiRejectionsResponse](t, testHandler.ListTuituiRejections,
		withURLParams(newRequestAs(testUserID, http.MethodGet,
			fmt.Sprintf("/api/workspaces/%s/tuitui/rejections?limit=2&offset=%d", testWorkspaceID, *first.NextOffset), nil),
			"id", testWorkspaceID),
		http.StatusOK)
	if len(second.Rejections) != 1 {
		t.Fatalf("second page = %+v, want the one remaining subject", second.Rejections)
	}
	if first.Rejections[0].ID == second.Rejections[0].ID {
		t.Errorf("pages overlap: %q on both", first.Rejections[0].ID)
	}
	if second.NextOffset != nil {
		t.Errorf("second page claims a continuation: %v", *second.NextOffset)
	}

	// Visibility: a plain member cannot see refusals attributed to a private
	// agent they may not open, even though they may read the workspace.
	memberPage := testutil.Decode[ListTuituiRejectionsResponse](t, testHandler.ListTuituiRejections,
		withURLParams(newRequestAs(memberID, http.MethodGet,
			"/api/workspaces/"+testWorkspaceID+"/tuitui/rejections", nil),
			"id", testWorkspaceID),
		http.StatusOK)
	if len(memberPage.Rejections) != 0 {
		t.Fatalf("member saw %+v for private agents", memberPage.Rejections)
	}
}

func TestListTuituiRejections_NothingPendingWhileAGateIsOpen(t *testing.T) {
	wireTuituiInstallService(t)
	agentID, ownerID, _ := privateAgentTestFixture(t)
	// The wildcard answer-everyone case: rows the bot really did write before the
	// operator opened it up stay in the audit table, and showing them would offer
	// ids that need no entry.
	bot := tuituiInstallByoWith(t, ownerID, agentID, "*", "*")
	seedTuituiRejection(t, bot.ID, "bob", "single_chat", "2026-01-06 10:00:00+00")
	seedTuituiRejection(t, bot.ID, "teams_t1_c2", "teams_post_create", "2026-01-05 10:00:00+00")

	page := testutil.Decode[ListTuituiRejectionsResponse](t, testHandler.ListTuituiRejections,
		withURLParams(newRequestAs(testUserID, http.MethodGet,
			"/api/workspaces/"+testWorkspaceID+"/tuitui/rejections", nil),
			"id", testWorkspaceID),
		http.StatusOK)
	if len(page.Rejections) != 0 {
		t.Fatalf("rejections = %+v, want none for a bot that refuses nobody", page.Rejections)
	}
}

func TestListTuituiRejections_BadPagingArguments(t *testing.T) {
	wireTuituiInstallService(t)
	tuituiRejectionsAs(t, testUserID, "?limit=0").Want(http.StatusBadRequest)
	tuituiRejectionsAs(t, testUserID, "?limit=abc").Want(http.StatusBadRequest)
	tuituiRejectionsAs(t, testUserID, "?offset=-1").Want(http.StatusBadRequest)
	tuituiRejectionsAs(t, testUserID, "?installation_id=not-a-uuid").Want(http.StatusBadRequest)
	// Over the cap is clamped, not refused: the client asked for a lot, and the
	// server answers with the most it will give.
	page := testutil.Decode[ListTuituiRejectionsResponse](t, testHandler.ListTuituiRejections,
		withURLParams(newRequestAs(testUserID, http.MethodGet,
			"/api/workspaces/"+testWorkspaceID+"/tuitui/rejections?limit=5000", nil),
			"id", testWorkspaceID),
		http.StatusOK)
	if len(page.Rejections) != 0 {
		t.Fatalf("unexpected rows %+v", page.Rejections)
	}
}

func TestListTuituiRejections_EmptyWhenNothingIsRefusedOrNothingIsConfigured(t *testing.T) {
	wireTuituiInstallService(t)
	agentID, ownerID, _ := privateAgentTestFixture(t)
	tuituiInstallByo(t, ownerID, agentID)

	// A bot with no gate refuses nobody, so the honest answer is an empty page.
	empty := testutil.Decode[ListTuituiRejectionsResponse](t, testHandler.ListTuituiRejections,
		withURLParams(newRequestAs(testUserID, http.MethodGet,
			"/api/workspaces/"+testWorkspaceID+"/tuitui/rejections", nil),
			"id", testWorkspaceID),
		http.StatusOK)
	if len(empty.Rejections) != 0 {
		t.Fatalf("rejections = %+v, want none", empty.Rejections)
	}
	if empty.Rejections == nil {
		t.Error("rejections must serialize as [], not null")
	}

	// Without a master key the channel never runs, so nothing CAN be refused:
	// that is a normal 200, matching how the sibling endpoints report it.
	previous := testHandler.TuituiInstall
	testHandler.TuituiInstall = nil
	t.Cleanup(func() { testHandler.TuituiInstall = previous })
	unconfigured := tuituiRejectionsAs(t, testUserID, "")
	unconfigured.Want(http.StatusOK)
	var body map[string]any
	if err := json.Unmarshal([]byte(unconfigured.Text()), &body); err != nil {
		t.Fatalf("decode unconfigured body: %v", err)
	}
	rows, ok := body["rejections"].([]any)
	if !ok || len(rows) != 0 {
		t.Fatalf("unconfigured rejections = %v, want []", body["rejections"])
	}
}

func TestListTuituiRejections_RequiresWorkspaceMembership(t *testing.T) {
	wireTuituiInstallService(t)
	agentID, ownerID, _ := privateAgentTestFixture(t)
	bot := tuituiInstallByoWith(t, ownerID, agentID, "alice", "778899")
	seedTuituiRejection(t, bot.ID, "bob", "single_chat", "2026-01-05 10:00:00+00")

	outsider := createTuituiNonMember(t, "tuitui-reject-non-member@multica.test")
	response := tuituiRejectionsAs(t, outsider, "")
	response.Want(http.StatusNotFound)
	// The denial must not leak the subject it would otherwise have listed.
	if strings.Contains(response.Text(), "bob") {
		t.Errorf("non-member response body mentions a refused subject: %s", response.Text())
	}
}
