package tuitui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
)

// allow_test.go covers the inbound allowlist: the stored shapes that keep
// "not configured" apart from "nobody", the decision rule, and the receive-loop
// gate that refuses a message before the engine is ever called.

// allowlistsFrom decodes a stored installation config the way a connection
// does, so every case below is stated in the JSON a row really holds.
func allowlistsFrom(t *testing.T, config string) Config {
	t.Helper()
	c, err := FromConfig(channel.Config{Type: TypeTuitui, Raw: json.RawMessage(config)})
	if err != nil {
		t.Fatalf("FromConfig(%s): %v", config, err)
	}
	return c
}

func TestAllowlistStoredShapesAreDistinct(t *testing.T) {
	cases := []struct {
		name      string
		config    string
		wantMode  AllowlistMode
		wantEntry string
		// allowsAny is the verdict an unknown id must get from this shape.
		allowsAny bool
	}{
		{
			// The shape every installation predating this feature has: no key.
			name:      "absent key imposes no restriction",
			config:    `{"app_id":"a","app_secret":"s"}`,
			wantMode:  AllowlistUnset,
			allowsAny: true,
		},
		{
			name:      "explicit null also imposes no restriction",
			config:    `{"app_id":"a","app_secret":"s","allow_from":null}`,
			wantMode:  AllowlistUnset,
			allowsAny: true,
		},
		{
			// The fail-closed shape: the operator cleared the field.
			name:     "explicit empty list denies everyone",
			config:   `{"app_id":"a","app_secret":"s","allow_from":[]}`,
			wantMode: AllowlistNone,
		},
		{
			name:      "wildcard allows anyone",
			config:    `{"app_id":"a","app_secret":"s","allow_from":["*"]}`,
			wantMode:  AllowlistAll,
			allowsAny: true,
		},
		{
			name:      "concrete ids only",
			config:    `{"app_id":"a","app_secret":"s","allow_from":["alice"]}`,
			wantMode:  AllowlistSome,
			wantEntry: "alice",
		},
		{
			// The reference tests "*" for membership before any id, so ids
			// stored beside it are inert and are normalized away.
			name:      "wildcard beside ids wins",
			config:    `{"app_id":"a","app_secret":"s","allow_from":["alice","*"]}`,
			wantMode:  AllowlistAll,
			allowsAny: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := allowlistsFrom(t, tc.config)
			if got := c.AllowFrom.Mode; got != tc.wantMode {
				t.Fatalf("Mode = %v, want %v", got, tc.wantMode)
			}
			if got := c.AllowFrom.Allows("anyone"); got != tc.allowsAny {
				t.Errorf("Allows(anyone) = %v, want %v", got, tc.allowsAny)
			}
			if tc.wantEntry != "" && !c.AllowFrom.Allows(tc.wantEntry) {
				t.Errorf("Allows(%q) = false, want true (entries %v)", tc.wantEntry, c.AllowFrom.Entries)
			}
			if tc.wantMode == AllowlistNone && c.AllowFrom.Allows("alice") {
				t.Error("an explicitly empty list must deny even the previously allowed account")
			}
		})
	}
}

func TestAllowlistStoredRoundTrip(t *testing.T) {
	// The stored JSON is what distinguishes the modes, so the write path must
	// emit exactly the three shapes and nothing in between.
	cases := []struct {
		name string
		list Allowlist
		// wantKey is the expected "allow_from" value, or "" for no key at all.
		wantKey string
	}{
		{"unconfigured writes no key", Allowlist{Mode: AllowlistUnset}, ""},
		{"none writes an empty array", Allowlist{Mode: AllowlistNone}, `[]`},
		{"all writes the bare wildcard", Allowlist{Mode: AllowlistAll}, `["*"]`},
		{"some writes its ids", Allowlist{Mode: AllowlistSome, Entries: []string{"a", "b"}}, `["a","b"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(installConfig{
				AppID:              "a",
				AppSecretEncrypted: "c",
				AllowFrom:          tc.list.Stored(),
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded map[string]json.RawMessage
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			value, present := decoded["allow_from"]
			if tc.wantKey == "" {
				if present {
					t.Fatalf("key must be absent, got %s", raw)
				}
				return
			}
			if !present {
				t.Fatalf("key must be present as %s, got %s", tc.wantKey, raw)
			}
			if strings.TrimSpace(string(value)) != tc.wantKey {
				t.Errorf("stored %s, want %s", value, tc.wantKey)
			}
			// Reading the row back must land on the same mode: that is the whole
			// point of the pointer shape.
			direct, _ := AllowlistsFromStoredConfig(raw)
			if got := direct.Mode; got != tc.list.Mode {
				t.Errorf("round-tripped mode = %v, want %v", got, tc.list.Mode)
			}
		})
	}
}

func TestConfigAllowlistDecisionMatrix(t *testing.T) {
	// Every row is the reference client's rule applied to one conversation:
	// DM → allow_from only; group/channel → group_allow_from first, then the
	// SENDER against allow_from.
	list := func(entries ...string) Allowlist {
		return Allowlist{Mode: AllowlistSome, Entries: entries}
	}
	cases := []struct {
		name   string
		allow  Allowlist
		group  Allowlist
		kind   chatKind
		chatID string
		sender string
		want   bool
	}{
		{
			name: "nothing configured answers everyone",
			kind: kindP2P, chatID: "bob", sender: "bob",
			want: true,
		},
		{
			name:  "a DM list naming alice admits alice",
			allow: list("alice"),
			kind:  kindP2P, chatID: "alice", sender: "alice",
			want: true,
		},
		{
			name:  "a DM list naming alice refuses bob",
			allow: list("alice"),
			kind:  kindP2P, chatID: "bob", sender: "bob",
			want: false,
		},
		{
			name:  "wildcard on the DM list admits anyone",
			allow: Allowlist{Mode: AllowlistAll},
			kind:  kindP2P, chatID: "bob", sender: "bob",
			want: true,
		},
		{
			name:  "an empty DM list admits nobody",
			allow: Allowlist{Mode: AllowlistNone},
			kind:  kindP2P, chatID: "alice", sender: "alice",
			want: false,
		},
		{
			name:  "a group on its own list is answered for any sender",
			allow: list("alice"), group: list("778899"),
			kind: kindGroup, chatID: "778899", sender: "stranger",
			want: true,
		},
		{
			name:  "an unlisted group falls back to the sender list",
			allow: list("alice"), group: list("778899"),
			kind: kindGroup, chatID: "445566", sender: "alice",
			want: true,
		},
		{
			name:  "an unlisted group from an unlisted sender is refused",
			allow: list("alice"), group: list("778899"),
			kind: kindGroup, chatID: "445566", sender: "bob",
			want: false,
		},
		{
			name:  "wildcard on the DM list opens groups too",
			allow: Allowlist{Mode: AllowlistAll}, group: list("778899"),
			kind: kindGroup, chatID: "445566", sender: "anyone",
			want: true,
		},
		{
			name:  "an unconfigured group list imposes nothing",
			allow: list("alice"),
			kind:  kindGroup, chatID: "445566", sender: "bob",
			want: true,
		},
		{
			// Both lists explicit and both empty: nothing may talk.
			name:  "an empty group list still falls back, to an empty DM list",
			allow: Allowlist{Mode: AllowlistNone}, group: Allowlist{Mode: AllowlistNone},
			kind: kindGroup, chatID: "778899", sender: "alice",
			want: false,
		},
		{
			name:  "an empty DM list cannot rescue an unlisted group",
			allow: Allowlist{Mode: AllowlistNone}, group: list("778899"),
			kind: kindGroup, chatID: "445566", sender: "alice",
			want: false,
		},
		{
			// Teams: the allowlist entry is the channel-scoped key, so every
			// thread and every post of that channel is covered.
			name:  "a teams post matches the channel-scoped entry",
			allow: list("alice"), group: list("teams_t1_c2"),
			kind: kindTeams, chatID: "teams_t1_c2_p9", sender: "bob",
			want: true,
		},
		{
			name:  "a teams post also matches its exact composite",
			allow: list("alice"), group: list("teams_t1_c2_p9"),
			kind: kindTeams, chatID: "teams_t1_c2_p9", sender: "bob",
			want: true,
		},
		{
			name:  "a teams post is not opened by a sibling channel",
			allow: list("alice"), group: list("teams_t1_c3"),
			kind: kindTeams, chatID: "teams_t1_c2_p9", sender: "bob",
			want: false,
		},
		{
			// No prefix matching on the composite: a bare team or a partial key
			// is just an id that happens to look like one.
			name:  "a bare team segment is not a prefix rule",
			allow: Allowlist{Mode: AllowlistNone}, group: list("teams_t1", "teams_t1_"),
			kind: kindTeams, chatID: "teams_t1_c2_p9", sender: "bob",
			want: false,
		},
		{
			name:  "wildcard opens teams as well",
			allow: list("alice"), group: Allowlist{Mode: AllowlistAll},
			kind: kindTeams, chatID: "teams_t1_c2_p9", sender: "bob",
			want: true,
		},
		{
			// A malformed composite (embedded underscores) keeps routing on the
			// teams_ prefix but contributes only itself to the match.
			name:  "a malformed composite matches only its exact id",
			allow: Allowlist{Mode: AllowlistNone}, group: list("teams_a_b_c_d"),
			kind: kindTeams, chatID: "teams_a_b_c_d", sender: "bob",
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{AllowFrom: tc.allow, GroupAllowFrom: tc.group}
			var got bool
			if tc.kind == kindP2P {
				got = c.allowsDirect(tc.sender)
			} else {
				got = c.allowsConversation(tc.kind, tc.chatID, tc.sender)
			}
			if got != tc.want {
				t.Errorf("allowed = %v, want %v (kind=%d chat=%q sender=%q)", got, tc.want, tc.kind, tc.chatID, tc.sender)
			}
		})
	}
}

func TestParseAllowlistCSV(t *testing.T) {
	cases := []struct {
		name  string
		field string
		want  []string
		err   bool
	}{
		{name: "single", field: "alice", want: []string{"alice"}},
		{name: "trims and drops empties", field: " alice , ,bob, ", want: []string{"alice", "bob"}},
		{name: "dedupes keeping order", field: "bob,alice,bob", want: []string{"bob", "alice"}},
		{name: "wildcard alone", field: "*", want: []string{"*"}},
		{name: "wildcard with spacing", field: " * ", want: []string{"*"}},
		{name: "wildcard wins over siblings", field: "alice,*,bob", want: []string{"*"}},
		{name: "empty field stores nothing", field: "   "},
		{name: "only separators stores nothing", field: ",,,"},
		{name: "numeric group id", field: "1234567890123456", want: []string{"1234567890123456"}},
		{name: "composite teams key", field: "teams_t1_c2", want: []string{"teams_t1_c2"}},
		{name: "over-long entry", field: strings.Repeat("x", MaxAllowlistEntryLen+1), err: true},
		{name: "control character", field: "alice\nbob", err: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAllowlistCSV(tc.field)
			if tc.err {
				if err == nil {
					t.Fatalf("ParseAllowlistCSV(%q) = %v, want an error", tc.field, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAllowlistCSV(%q): %v", tc.field, err)
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("ParseAllowlistCSV(%q) = %q, want %q", tc.field, got, tc.want)
			}
		})
	}
	// The entry cap is a write-path guard, so it has to reject rather than
	// silently truncate.
	if _, err := ParseAllowlistCSV(strings.Join(manyIDs(MaxAllowlistEntries+1), ",")); err == nil {
		t.Error("an over-sized list must be rejected")
	}
}

func manyIDs(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("user-%d", i))
	}
	return out
}

func TestAllowlistAuditKey(t *testing.T) {
	cases := []struct {
		name   string
		kind   chatKind
		chatID string
		want   string
	}{
		// A DM's chat id IS the account, which is the id allow_from takes.
		{"direct", kindP2P, "alice", "alice"},
		{"group", kindGroup, "1234567890123456", "1234567890123456"},
		{"teams is recorded without its thread", kindTeams, "teams_t1_c2_p9", "teams_t1_c2"},
		{"teams already at channel scope", kindTeams, "teams_t1_c2", "teams_t1_c2"},
		{"malformed teams keeps its exact id", kindTeams, "teams_a_b_c_d", "teams_a_b_c_d"},
		{"empty", kindGroup, "  ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := allowlistAuditKey(tc.kind, tc.chatID); got != tc.want {
				t.Errorf("allowlistAuditKey(%d, %q) = %q, want %q", tc.kind, tc.chatID, got, tc.want)
			}
		})
	}
}

// ---- the receive-loop gate ----

// recordedAuditor stands in for the shared engine.Auditor so a test can see
// what a refusal wrote without a database.
type recordedAuditor struct {
	calls []auditedDrop
	err   error
}

// auditedDrop is one RecordDrop call: which installation, which reason, and
// which subject id the row carried.
type auditedDrop struct {
	InstallationID pgtype.UUID
	Reason         engine.DropReason
}

func (a *recordedAuditor) RecordDrop(_ context.Context, instID pgtype.UUID, _ channel.InboundMessage, reason engine.DropReason) error {
	a.calls = append(a.calls, auditedDrop{InstallationID: instID, Reason: reason})
	return a.err
}

func allowGateChannel(t *testing.T, cfg Config, delivered *[]channel.InboundMessage, auditor engine.Auditor) *tuituiChannel {
	t.Helper()
	return New(cfg, func(_ context.Context, msg channel.InboundMessage) error {
		*delivered = append(*delivered, msg)
		return nil
	}, ConfigDeps{
		InstID: pgtype.UUID{Bytes: [16]byte{7}, Valid: true},
		Audit:  auditor,
	})
}

func frameFor(t *testing.T, event, account, data string) *wsFrame {
	t.Helper()
	return frameForID(t, "ev-9", event, account, data)
}

// frameForID builds a frame with an explicit event_id, which matters because
// handleFrame dedups frames by it: two fixtures sharing one id would exercise
// the redelivery path instead of the gate under test.
func frameForID(t *testing.T, eventID, event, account, data string) *wsFrame {
	t.Helper()
	raw := `{"event_id":"` + eventID + `","body":{"event":"` + event +
		`","user_account":"` + account + `","user_name":"` + strings.ToUpper(account[:1]) +
		`","data":` + data + `}}`
	var f wsFrame
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return &f
}

func TestTuituiChannelRefusesOutsideAllowlist(t *testing.T) {
	cfg := baseConfig()
	cfg.AllowFrom = Allowlist{Mode: AllowlistSome, Entries: []string{"alice"}}
	cfg.GroupAllowFrom = Allowlist{Mode: AllowlistSome, Entries: []string{"778899"}}

	cases := []struct {
		name    string
		frame   *wsFrame
		wantRef bool // refused → audited, and never delivered
		// wantID is the subject id the audit row has to carry: the exact string
		// the operator needs to paste, which is the whole reason refusals are
		// audited instead of only logged.
		wantID string
	}{
		{
			name:    "DM from the listed account is delivered",
			frame:   frameFor(t, eventSingleChat, "alice", `{"msgid":"m-1","msg_type":"text","text":"hi"}`),
			wantRef: false,
		},
		{
			name:    "DM from anyone else is refused and audited by account",
			frame:   frameFor(t, eventSingleChat, "bob", `{"msgid":"m-2","msg_type":"text","text":"hi"}`),
			wantRef: true, wantID: "bob",
		},
		{
			name:    "listed group is delivered for any sender",
			frame:   frameFor(t, eventGroupChat, "stranger", `{"msgid":"m-3","msg_type":"text","text":"hi","group_id":"778899","at_me":true}`),
			wantRef: false,
		},
		{
			name:    "unlisted group from an unlisted sender is refused by group id",
			frame:   frameFor(t, eventGroupChat, "bob", `{"msgid":"m-4","msg_type":"text","text":"hi","group_id":"445566","at_me":true}`),
			wantRef: true, wantID: "445566",
		},
		{
			name:    "unlisted group from the listed sender falls back and is delivered",
			frame:   frameFor(t, eventGroupChat, "alice", `{"msgid":"m-5","msg_type":"text","text":"hi","group_id":"445566","at_me":true}`),
			wantRef: false,
		},
		{
			name:    "a teams post is refused under its channel-scoped key",
			frame:   frameFor(t, eventTeamsPostCreate, "bob", `{"team_id":"t1","channel_id":"c2","post_id":"p9","content":"hi"}`),
			wantRef: true, wantID: "teams_t1_c2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var delivered []channel.InboundMessage
			auditor := &recordedAuditor{}
			ch := allowGateChannel(t, cfg, &delivered, auditor)

			ch.handleFrame(context.Background(), tc.frame)

			if tc.wantRef {
				if len(delivered) != 0 {
					t.Fatalf("a refused message must never reach the engine handler, got %d delivery(ies)", len(delivered))
				}
				if len(auditor.calls) != 1 {
					t.Fatalf("refusal must write exactly one audit row, got %d", len(auditor.calls))
				}
				if got := auditor.calls[0].Reason; got != engine.DropReason(AllowlistReason) {
					t.Errorf("drop reason = %q, want %q", got, AllowlistReason)
				}
				if auditor.calls[0].InstallationID.Valid == false {
					t.Error("the audit row must carry the installation it was refused by")
				}
				return
			}
			if len(delivered) != 1 {
				t.Fatalf("an allowed message must be delivered once, got %d", len(delivered))
			}
			if len(auditor.calls) != 0 {
				t.Errorf("an allowed message must not be audited as a drop: %+v", auditor.calls)
			}
		})
	}
}

// TestTuituiChannelAuditSubjectCarriesAllowlistKey checks the audited chat id
// directly: what the panel offers back must be pasteable as-is.
func TestTuituiChannelAuditSubjectCarriesAllowlistKey(t *testing.T) {
	cfg := baseConfig()
	cfg.AllowFrom = Allowlist{Mode: AllowlistNone}
	cfg.GroupAllowFrom = Allowlist{Mode: AllowlistNone}
	var delivered []channel.InboundMessage
	auditor := &captureMsgAuditor{}
	ch := allowGateChannel(t, cfg, &delivered, auditor)

	ch.handleFrame(context.Background(),
		frameFor(t, eventTeamsPostCreate, "bob", `{"team_id":"t1","channel_id":"c2","post_id":"p9","content":"hi"}`))
	if len(delivered) != 0 {
		t.Fatal("an explicitly empty DM list must refuse this sender")
	}
	if len(auditor.messages) != 1 {
		t.Fatalf("audit calls = %d, want 1", len(auditor.messages))
	}
	got := auditor.messages[0]
	if got.Source.ChatID != "teams_t1_c2" {
		t.Errorf("audited chat id = %q, want the channel-scoped key teams_t1_c2 (not the thread id)", got.Source.ChatID)
	}
	if got.Source.SenderID != "bob" || got.MessageID != "p9" {
		t.Errorf("audited routing = %q / %q", got.Source.SenderID, got.MessageID)
	}
	if len(got.Raw) == 0 {
		t.Error("the audit row keeps the event type behind the refusal, so Raw must survive")
	}
}

type captureMsgAuditor struct {
	messages []channel.InboundMessage
}

func (a *captureMsgAuditor) RecordDrop(_ context.Context, _ pgtype.UUID, msg channel.InboundMessage, _ engine.DropReason) error {
	a.messages = append(a.messages, msg)
	return nil
}

func TestTuituiChannelWithoutAllowlistsIsUnrestricted(t *testing.T) {
	// The decision this feature lives or dies by: a row stored before it
	// existed has no keys, and must keep answering exactly as it did — if this
	// test goes red, every deployed bot just went silent.
	var delivered []channel.InboundMessage
	auditor := &recordedAuditor{}
	ch := allowGateChannel(t, baseConfig(), &delivered, auditor)

	ch.handleFrame(context.Background(),
		frameForID(t, "ev-1", eventSingleChat, "whoever", `{"msgid":"m-1","msg_type":"text","text":"hi"}`))
	ch.handleFrame(context.Background(),
		frameForID(t, "ev-2", eventGroupChat, "whoever", `{"msgid":"m-2","msg_type":"text","text":"hi","group_id":"445566","at_me":true}`))

	if len(delivered) != 2 {
		t.Fatalf("unconfigured lists must deliver both messages, got %d", len(delivered))
	}
	if len(auditor.calls) != 0 {
		t.Errorf("nothing may be audited as a refusal here: %+v", auditor.calls)
	}
}

func TestTuituiChannelRefusesEvenWithNoAuditSink(t *testing.T) {
	// The audit write is there to help the operator fill the list in; it can
	// never be what decides a refusal. A channel built without an auditor (or
	// without a resolvable installation) still refuses.
	cfg := baseConfig()
	cfg.AllowFrom = Allowlist{Mode: AllowlistNone}
	var delivered []channel.InboundMessage
	ch := New(cfg, func(_ context.Context, _ channel.InboundMessage) error {
		t.Error("handler must not run for a refused message")
		return nil
	}, ConfigDeps{})

	ch.handleFrame(context.Background(),
		frameFor(t, eventSingleChat, "bob", `{"msgid":"m-1","msg_type":"text","text":"hi"}`))
	if len(delivered) != 0 {
		t.Fatal("refusal must hold without an auditor")
	}
}

func TestTuituiChannelAuditFailureStillRefuses(t *testing.T) {
	// A failing audit write must not become a delivery, and must not break the
	// receive loop either: the refusal already happened.
	cfg := baseConfig()
	cfg.AllowFrom = Allowlist{Mode: AllowlistNone}
	var delivered []channel.InboundMessage
	auditor := &recordedAuditor{err: fmt.Errorf("audit insert rejected")}
	ch := allowGateChannel(t, cfg, &delivered, auditor)

	ch.handleFrame(context.Background(),
		frameFor(t, eventSingleChat, "bob", `{"msgid":"m-1","msg_type":"text","text":"hi"}`))
	if len(delivered) != 0 {
		t.Fatal("a failed audit write must not deliver the refused message")
	}
	if len(auditor.calls) != 1 {
		t.Fatalf("the auditor must still have been asked once, got %d", len(auditor.calls))
	}
}
