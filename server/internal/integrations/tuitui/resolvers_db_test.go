package tuitui

// resolvers_db_test.go drives the Tuitui ResolverSet through the real
// engine.Router over a real Postgres. An inbound frame is normalized by the
// adapter itself (normalizeEvent, including the stamped app_id) and handed
// to Router.Handle; the assertions are the production questions: does a
// conversation get a durable channel_chat_session_binding with the right
// chat_type (teams → group, single_chat → p2p), does an unknown user get a
// minted binding token plus a delivered redeem link, and does a bound user
// stop repeating that prompt. Outbound sends never leave the process: the
// stored installation config points at an httptest TLS server, so the same
// decode path also proves the send URL is built from the row's host + port.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// tuituiTestPool backs the DB-backed tests; TestMain leaves it nil (each
// test then skips itself) when no database is reachable, so the package's
// unit tests keep running anywhere.
var tuituiTestPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err == nil {
		var migrated bool
		if probeErr := pool.QueryRow(ctx,
			`SELECT to_regclass('public.channel_installation') IS NOT NULL`).Scan(&migrated); probeErr != nil {
			err = probeErr
		} else if !migrated {
			err = errors.New("database not migrated (channel_installation missing)")
		}
	}
	if err != nil {
		fmt.Printf("DB-backed Tuitui tests skipped: %v\n", err)
		if pool != nil {
			pool.Close()
		}
		os.Exit(m.Run())
	}
	tuituiTestPool = pool
	code := m.Run()
	pool.Close()
	os.Exit(code)
}

func testDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mustRouteUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return u
}

// ---- outbound send stub ----

type stubSendRequest struct {
	Path   string
	AppID  string
	Secret string
	Body   sendPayload
}

type tuituiStubSend struct {
	t        *testing.T
	mu       sync.Mutex
	requests []stubSendRequest
}

func (s *tuituiStubSend) all() []stubSendRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubSendRequest(nil), s.requests...)
}

func (s *tuituiStubSend) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var p sendPayload
	if err := json.Unmarshal(body, &p); err != nil {
		s.t.Errorf("stub: decode send payload: %v (%s)", err, body)
	}
	q := r.URL.Query()
	s.mu.Lock()
	s.requests = append(s.requests, stubSendRequest{
		Path: r.URL.Path, AppID: q.Get("appid"), Secret: q.Get("secret"), Body: p,
	})
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"errcode":0,"msgid":"out-1"}`)
}

// newSendStub records every posted send. The port is what a test stores in
// the installation config, so a recorded request proves the resolver /
// replier / subscriber build the send URL from the row, not a constant.
func newSendStub(t *testing.T) (*httptest.Server, *tuituiStubSend, int) {
	t.Helper()
	stub := &tuituiStubSend{t: t}
	srv := httptest.NewTLSServer(http.HandlerFunc(stub.handler))
	t.Cleanup(srv.Close)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	return srv, stub, port
}

// ---- scenario fixture ----

type tuituiRouteFixture struct {
	t    *testing.T
	fx   *testutil.Fixture
	pool *pgxpool.Pool
	q    *db.Queries

	appID   string
	sender  string
	stub    *tuituiStubSend
	stubTLS *http.Client

	workspaceIDStr  string
	userIDStr       string
	agentIDStr      string
	runtimeIDStr    string
	installationStr string

	workspaceID  pgtype.UUID
	userID       pgtype.UUID
	agentID      pgtype.UUID
	installation pgtype.UUID
}

func newTuituiRouteFixture(t *testing.T) *tuituiRouteFixture {
	t.Helper()
	if tuituiTestPool == nil {
		t.Skip("database not available")
	}
	srv, stub, port := newSendStub(t)
	suffix := strings.ToLower(uuid.NewString()[:8])
	fixture := testutil.New(tuituiTestPool, "", "")

	fixture.UserID = fixture.User(t, "Tuitui Route Owner "+suffix, "tuitui-route-"+suffix+"@multica.test")
	fixture.WorkspaceID = fixture.Workspace(t, "Tuitui Route "+suffix, "tuitui-route-"+suffix)
	fixture.Member(t, fixture.WorkspaceID, fixture.UserID, "owner")
	runtimeID := fixture.Runtime(t, "tuitui-runtime-"+suffix)
	agentID := fixture.Agent(t, "tuitui-agent-"+suffix, runtimeID)

	appID := "tuitui-route-" + suffix
	cfgJSON, err := json.Marshal(installConfig{
		AppID: appID, AppSecret: "sec-1", Host: "127.0.0.1", Port: port,
	})
	if err != nil {
		t.Fatalf("marshal install config: %v", err)
	}
	installationID := fixture.Insert(t, "channel_installation", testutil.Cols{
		"workspace_id":      fixture.WorkspaceID,
		"agent_id":          agentID,
		"channel_type":      string(TypeTuitui),
		"config":            testutil.Raw("'" + string(cfgJSON) + "'::jsonb"),
		"installer_user_id": fixture.UserID,
		"status":            "active",
	})

	// Rows the Router / ChatSession write durably carry no foreign key
	// back to the installation, so the Fixture's per-row cleanup cannot
	// reach them; delete the engine-written sets explicitly.
	fixture.Cleanup(t, `DELETE FROM chat_message WHERE chat_session_id IN (SELECT id FROM chat_session WHERE agent_id = $1)`, agentID)
	fixture.Cleanup(t, `DELETE FROM channel_task_delivery WHERE installation_id = $1`, installationID)
	fixture.Cleanup(t, `DELETE FROM channel_inbound_message_dedup WHERE installation_id = $1`, installationID)
	fixture.Cleanup(t, `DELETE FROM channel_inbound_audit WHERE installation_id = $1`, installationID)
	fixture.Cleanup(t, `DELETE FROM channel_binding_token WHERE installation_id = $1`, installationID)
	fixture.Cleanup(t, `DELETE FROM channel_user_binding WHERE installation_id = $1`, installationID)
	fixture.Cleanup(t, `DELETE FROM channel_media_pending_object WHERE installation_id = $1`, installationID)
	fixture.Cleanup(t, `DELETE FROM channel_chat_context_generation WHERE chat_session_id IN (SELECT id FROM chat_session WHERE agent_id = $1)`, agentID)
	fixture.Cleanup(t, `DELETE FROM channel_chat_session_binding WHERE installation_id = $1`, installationID)
	fixture.Cleanup(t, `DELETE FROM chat_session WHERE agent_id = $1`, agentID)

	return &tuituiRouteFixture{
		t: t, fx: fixture, pool: tuituiTestPool, q: db.New(tuituiTestPool),
		appID: appID, sender: "alice-" + suffix,
		stub: stub, stubTLS: srv.Client(),
		workspaceIDStr: fixture.WorkspaceID, userIDStr: fixture.UserID,
		agentIDStr: agentID, runtimeIDStr: runtimeID, installationStr: installationID,
		workspaceID:  mustRouteUUID(t, fixture.WorkspaceID),
		userID:       mustRouteUUID(t, fixture.UserID),
		agentID:      mustRouteUUID(t, agentID),
		installation: mustRouteUUID(t, installationID),
	}
}

// bindSender marks the fixture's Tuitui sender as a bound Multica user —
// the row the redemption endpoint creates and the identity resolver reads.
func (f *tuituiRouteFixture) bindSender() {
	f.t.Helper()
	f.fx.Insert(f.t, "channel_user_binding", testutil.Cols{
		"workspace_id":    f.workspaceIDStr,
		"multica_user_id": f.userIDStr,
		"installation_id": f.installationStr,
		"channel_type":    string(TypeTuitui),
		"channel_user_id": f.sender,
	})
}

// replier wires the real outbound replier against the send stub and a fixed
// app URL, so NeedsBinding prompts are observable end to end.
func (f *tuituiRouteFixture) replier() *OutboundReplier {
	return NewOutboundReplier(OutboundReplierConfig{
		Binding:    NewBindingTokenService(f.q, f.pool),
		Decrypt:    nil, // the fixture stores the secret plaintext
		HTTPClient: f.stubTLS,
		AppURL:     "https://app.tuitui.test",
		Logger:     testDiscardLogger(),
	})
}

// router builds a Router over the real ResolverSet with the Media slot
// unwired (the state of a deployment without an object-storage backend).
// The task enqueuer and issue creator are fakes: this suite asserts what
// the pipeline persists and delivers, not what the agent runtime would run.
func (f *tuituiRouteFixture) router(replier engine.OutboundReplier, tasks *fakeTaskEnqueuer) *engine.Router {
	return f.routerWithMedia(replier, tasks, nil)
}

func (f *tuituiRouteFixture) routerWithMedia(replier engine.OutboundReplier, tasks *fakeTaskEnqueuer, media engine.MediaResolver) *engine.Router {
	r := engine.NewRouter(fakeIssueCreator{}, tasks, f.q, engine.RouterConfig{Logger: testDiscardLogger()})
	r.Register(TypeTuitui, NewTuituiResolverSet(f.q, f.pool, replier, media))
	return r
}

// issueRouter builds a Router over the real ResolverSet with the real
// IssueService in place of the fake, so an /issue command writes a real
// issue row and the test can ask the database what provenance that row
// carries. The outbound replier stays nil: this suite reads the row, it
// does not assert the chat notice.
func (f *tuituiRouteFixture) issueRouter(tasks *fakeTaskEnqueuer) *engine.Router {
	bus := events.New()
	issueService := service.NewIssueService(f.q, f.pool, bus, nil,
		&service.TaskService{Queries: f.q, TxStarter: f.pool, Bus: bus})
	r := engine.NewRouter(issueService, tasks, f.q, engine.RouterConfig{Logger: testDiscardLogger()})
	r.Register(TypeTuitui, NewTuituiResolverSet(f.q, f.pool, nil, nil))
	return r
}

func drainRouter(t *testing.T, r *engine.Router) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !r.Drain(ctx) {
		t.Fatal("router.Drain did not finish: a detached reply or media goroutine is stuck")
	}
}

// ---- inbound builders ----

// tuituiFrame builds one callback frame exactly as the platform delivers it
// (a fresh event_id so the bounded transient dedup never collapses distinct
// messages in a test).
func tuituiFrame(t *testing.T, event, sender, data string) *wsFrame {
	t.Helper()
	raw := `{"event_id":"` + uuid.NewString() + `","body":{"event":"` + event +
		`","user_account":"` + sender + `","user_name":"` + sender +
		`","data":` + data + `}}`
	var f wsFrame
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return &f
}

func (f *tuituiRouteFixture) inbound(t *testing.T, event, sender, data string) channel.InboundMessage {
	t.Helper()
	n, ok := normalizeEvent(tuituiFrame(t, event, sender, data), f.appID)
	if !ok {
		t.Fatalf("frame did not normalize: event=%s data=%s", event, data)
	}
	return n.msg
}

// ---- Router fakes ----

type fakeIssueCreator struct{}

func (fakeIssueCreator) Create(context.Context, service.IssueCreateParams, service.IssueCreateOpts) (service.IssueCreateResult, error) {
	return service.IssueCreateResult{}, errors.New("tuitui route test: issue creation is not exercised")
}
func (fakeIssueCreator) PublishAttachmentsChanged(context.Context, db.Issue, pgtype.UUID) {}

type recordedChatRun struct {
	sessionID pgtype.UUID
	initiator pgtype.UUID
	bindingID pgtype.UUID
}

type fakeTaskEnqueuer struct {
	mu    sync.Mutex
	calls []recordedChatRun
}

func (e *fakeTaskEnqueuer) EnqueueChannelChatTask(_ context.Context, session db.ChatSession, initiatorUserID pgtype.UUID, _ bool, _ int64, bindingID pgtype.UUID, _ int64) (db.AgentTaskQueue, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, recordedChatRun{
		sessionID: session.ID, initiator: initiatorUserID, bindingID: bindingID,
	})
	return db.AgentTaskQueue{}, nil
}

func (e *fakeTaskEnqueuer) recorded() []recordedChatRun {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]recordedChatRun(nil), e.calls...)
}

func (e *fakeTaskEnqueuer) PrepareChatTaskEnqueue(context.Context, pgtype.UUID, pgtype.UUID) (service.PreparedChatTaskEnqueue, error) {
	return service.PreparedChatTaskEnqueue{}, nil
}
func (e *fakeTaskEnqueuer) EnqueuePreparedChannelChatTaskInTx(context.Context, pgx.Tx, db.ChatSession, pgtype.UUID, bool, int64, service.PreparedChatTaskEnqueue) (db.AgentTaskQueue, error) {
	panic("tuitui route test: prepared enqueue is not exercised")
}
func (e *fakeTaskEnqueuer) FinalizeChatTaskEnqueue(context.Context, db.AgentTaskQueue) {}
func (e *fakeTaskEnqueuer) PromoteChannelChatTasksIfMediaReady(context.Context, pgtype.UUID) error {
	return nil
}
func (e *fakeTaskEnqueuer) PromoteDeferredChannelIssueTask(context.Context, pgtype.UUID) error {
	return nil
}

// ---- tests ----

// TestTuituiRouterIngestsBoundP2PUser is the incident's happy path: one
// single_chat frame, fully routed, lands a p2p binding and an agent run.
func TestTuituiRouterIngestsBoundP2PUser(t *testing.T) {
	f := newTuituiRouteFixture(t)
	f.bindSender()
	tasks := &fakeTaskEnqueuer{}
	r := f.router(f.replier(), tasks)

	msg := f.inbound(t, eventSingleChat, f.sender, `{"msgid":"m-p2p-1","msg_type":"text","text":"hello bot"}`)
	if err := r.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	drainRouter(t, r)

	var chatType, chatID, channelType, sessionID string
	f.fx.QueryRow(t, `
		SELECT chat_type, channel_chat_id, channel_type, chat_session_id::text
		FROM channel_chat_session_binding WHERE installation_id = $1`,
		f.installationStr).Scan(&chatType, &chatID, &channelType, &sessionID)
	if chatType != "p2p" {
		t.Errorf("binding chat_type = %q, want p2p", chatType)
	}
	if chatID != f.sender {
		t.Errorf("binding channel_chat_id = %q, want the sender account %q (single_chat key)", chatID, f.sender)
	}
	if channelType != string(TypeTuitui) {
		t.Errorf("binding channel_type = %q", channelType)
	}
	if got := f.fx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID); got != 1 {
		t.Errorf("chat_message rows = %d, want the one ingested turn", got)
	}
	calls := tasks.recorded()
	if len(calls) != 1 || calls[0].sessionID != mustRouteUUID(t, sessionID) {
		t.Errorf("run enqueues = %+v, want one for session %s", calls, sessionID)
	}
	if !calls[0].bindingID.Valid {
		t.Errorf("run enqueue lost its delivery fence (binding id): %+v", calls[0])
	}
	// A bound user must not mint binding tokens.
	if got := f.fx.Count(t, `SELECT count(*) FROM channel_binding_token WHERE installation_id = $1`, f.installationStr); got != 0 {
		t.Errorf("binding tokens = %d, want 0 for a bound sender", got)
	}
}

// TestTuituiRouterTeamsBindingIsGroupAndParsesFromPrefix covers the hard
// constraint: a teams post rides ChatTypeGroup (the DB CHECK allows only
// p2p|group) while the stored key keeps the "teams_" prefix the outbound
// path reverse-parses — durable across restarts because it never consults
// the in-memory kind map.
func TestTuituiRouterTeamsBindingIsGroupAndParsesFromPrefix(t *testing.T) {
	f := newTuituiRouteFixture(t)
	f.bindSender()
	tasks := &fakeTaskEnqueuer{}
	r := f.router(f.replier(), tasks)

	msg := f.inbound(t, eventTeamsPostCreate, f.sender,
		`{"team_id":"eng","channel_id":"release","post_id":"post-9","parent_id":"0","content":"ship it"}`)
	if msg.Source.ChatID != "teams_eng_release_post-9" {
		t.Fatalf("composite teams id = %q", msg.Source.ChatID)
	}
	if err := r.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	drainRouter(t, r)

	var chatType, chatID string
	f.fx.QueryRow(t, `
		SELECT chat_type, channel_chat_id
		FROM channel_chat_session_binding WHERE installation_id = $1`,
		f.installationStr).Scan(&chatType, &chatID)
	if chatType != "group" {
		t.Errorf("teams binding chat_type = %q, want group (DB CHECK p2p|group)", chatType)
	}
	if chatID != "teams_eng_release_post-9" {
		t.Errorf("teams binding key = %q, want the composite id with the teams_ prefix", chatID)
	}
	// The stored key must reverse-parse for the outbound path.
	team, channelID, thread, ok := splitTeamsChatID(chatID)
	if !ok || team != "eng" || channelID != "release" || thread != "post-9" {
		t.Errorf("reverse parse = %q/%q/%q ok=%v", team, channelID, thread, ok)
	}
	if calls := tasks.recorded(); len(calls) != 1 {
		t.Errorf("run enqueues = %+v, want one", calls)
	}
}

// TestTuituiRouterUnknownUserGetsBindingLink is the second half of the
// incident: with zero channel_user_binding rows the sender must receive a
// redeem link over the platform, minted single-use, delivered privately to
// their user account — never into the conversation.
func TestTuituiRouterUnknownUserGetsBindingLink(t *testing.T) {
	f := newTuituiRouteFixture(t)
	r := f.router(f.replier(), &fakeTaskEnqueuer{})

	msg := f.inbound(t, eventSingleChat, f.sender, `{"msgid":"m-bind-1","msg_type":"text","text":"hello?"}`)
	if err := r.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	drainRouter(t, r)

	var tokenUser string
	f.fx.QueryRow(t, `
		SELECT channel_user_id,
		       (now() < expires_at) AS unexpired
		FROM channel_binding_token WHERE installation_id = $1`,
		f.installationStr).Scan(&tokenUser, new(bool))
	if tokenUser != f.sender {
		t.Errorf("token channel_user_id = %q, want %q", tokenUser, f.sender)
	}
	// NeedsBinding is terminal for this message: nothing was ingested.
	if got := f.fx.Count(t, `SELECT count(*) FROM channel_chat_session_binding WHERE installation_id = $1`, f.installationStr); got != 0 {
		t.Errorf("chat session bindings = %d, want none until the user binds", got)
	}

	reqs := f.stub.all()
	if len(reqs) != 1 {
		t.Fatalf("sends = %d, want exactly the one binding prompt: %+v", len(reqs), reqs)
	}
	req := reqs[0]
	if req.Path != "/robot"+pathSend {
		t.Errorf("prompt path = %q, want /robot%s (host:port came from the stored config)", req.Path, pathSend)
	}
	if req.AppID != f.appID || req.Secret != "sec-1" {
		t.Errorf("query auth = %q/%q", req.AppID, req.Secret)
	}
	if req.Body.MsgType != "text" || req.Body.Text == nil {
		t.Fatalf("prompt payload = %+v", req.Body)
	}
	if len(req.Body.ToUsers) != 1 {
		t.Fatalf("prompt recipients = %v, want a private tousers send", req.Body.ToUsers)
	}
	var recipient string
	if err := json.Unmarshal(req.Body.ToUsers[0], &recipient); err != nil || recipient != f.sender {
		t.Errorf("prompt recipient = %v, want sender %q", req.Body.ToUsers[0], f.sender)
	}
	content := req.Body.Text.Content
	if !strings.Contains(content, "[link your account](https://app.tuitui.test/tuitui/bind?token=") {
		t.Errorf("prompt text = %q, want the redeem link on the app URL", content)
	}
	if !strings.Contains(content, "15 minutes") {
		t.Errorf("prompt text = %q, want the expiry note", content)
	}
}

// TestTuituiRouterBoundUserGetsNoPrompts is the repeat-suppression proof:
// two distinct ingested messages from a bound sender mint zero tokens and
// produce zero sends, where the same shape unbound produced one each.
func TestTuituiRouterBoundUserGetsNoPrompts(t *testing.T) {
	f := newTuituiRouteFixture(t)
	f.bindSender()
	r := f.router(f.replier(), &fakeTaskEnqueuer{})
	ctx := context.Background()

	for i, text := range []string{"first", "second"} {
		msg := f.inbound(t, eventSingleChat, f.sender,
			fmt.Sprintf(`{"msgid":"m-noprompt-%d","msg_type":"text","text":%q}`, i, text))
		if err := r.Handle(ctx, msg); err != nil {
			t.Fatalf("Handle %d: %v", i, err)
		}
	}
	drainRouter(t, r)

	if got := f.fx.Count(t, `SELECT count(*) FROM channel_binding_token WHERE installation_id = $1`, f.installationStr); got != 0 {
		t.Errorf("binding tokens = %d, want 0", got)
	}
	if reqs := f.stub.all(); len(reqs) != 0 {
		t.Errorf("sends = %d, want 0 — bound users get chat, not prompts: %+v", len(reqs), reqs)
	}
	if got := f.fx.Count(t, `SELECT count(*) FROM channel_chat_session_binding WHERE installation_id = $1`, f.installationStr); got != 1 {
		t.Errorf("chat session bindings = %d, want one session for both messages", got)
	}
}

// TestTuituiRouterNilMediaResolverTolerated pins the store-less deployment
// shape: with the Media slot unwired the Router must ingest a media frame
// without ever touching the absent resolver — no panic, no deferred media
// state — and the durable body must still carry only the placeholder (the
// signed url stays in Raw, which nothing renders).
func TestTuituiRouterNilMediaResolverTolerated(t *testing.T) {
	f := newTuituiRouteFixture(t)
	f.bindSender()
	set := NewTuituiResolverSet(f.q, f.pool, nil, nil)
	if set.Media != nil {
		t.Fatalf("set has Media=%v, want nil when no resolver is supplied", set.Media)
	}
	if set.Typing != nil {
		t.Fatalf("set has Typing=%v, want nil: the platform protocol has no typing capability", set.Typing)
	}
	r := f.router(nil, &fakeTaskEnqueuer{})

	msg := f.inbound(t, eventSingleChat, f.sender,
		`{"msgid":"m-img-1","msg_type":"image","images":["https://media.tuitui.test/a.png?sig=topsecret"]}`)
	if msg.Type != channel.MsgTypeImage || len(msg.MediaRefs) != 0 {
		t.Fatalf("normalized media msg = %+v", msg)
	}
	if err := r.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle with nil Media resolver: %v", err)
	}
	drainRouter(t, r)

	if got := f.fx.Count(t, `
		SELECT count(*) FROM chat_message m
		JOIN channel_chat_session_binding b ON b.chat_session_id = m.chat_session_id
		WHERE b.installation_id = $1`, f.installationStr); got != 1 {
		t.Errorf("ingested media messages = %d, want 1", got)
	}
	var body string
	f.fx.QueryRow(t, `
		SELECT m.content FROM chat_message m
		JOIN channel_chat_session_binding b ON b.chat_session_id = m.chat_session_id
		WHERE b.installation_id = $1`, f.installationStr).Scan(&body)
	if body != "[图片]" {
		t.Errorf("durable body = %q, want the bare placeholder", body)
	}
	// Without a resolver nothing may claim a media deadline: the deferred
	// run machinery must stay untouched.
	if got := f.fx.Count(t, `
		SELECT count(*) FROM chat_message m
		JOIN channel_chat_session_binding b ON b.chat_session_id = m.chat_session_id
		WHERE b.installation_id = $1 AND m.channel_media_pending_until IS NOT NULL`,
		f.installationStr); got != 0 {
		t.Errorf("media-pending rows without a resolver = %d, want 0", got)
	}
}

// TestTuituiRouterInboundImageBindsAttachmentAndIntent is the full media
// route over the real ResolverSet: an image frame whose signed url points
// at an httptest host, resolved by the real resolver + real
// engine.NewDBMediaIntentLedger against the fixture database. It asserts
// the end-to-end contract each piece was written for — the session binding
// exists, the attachment row was created from the resolver's object with
// the storage URL (never the signed platform URL), the intent ledger row
// was claimed (deleted) by the bind transaction, the pending deadline
// cleared, and the durable body never shows the raw link.
func TestTuituiRouterInboundImageBindsAttachmentAndIntent(t *testing.T) {
	f := newTuituiRouteFixture(t)
	f.bindSender()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(mediaTestPNG)
	}))
	t.Cleanup(srv.Close)

	// The engine writes attachment + pending-intent rows the Fixture's
	// per-row cleanup cannot reach.
	f.fx.Cleanup(t, `DELETE FROM attachment WHERE workspace_id = $1`, f.workspaceIDStr)

	store := newFakeMediaStore()
	resolver := NewMediaResolver(store, engine.NewDBMediaIntentLedger(f.q), testDiscardLogger()).(*mediaResolver)
	// Injectable client: this test must never dial, and httptest's plain
	// loopback server is exactly what the production guard refuses.
	resolver.fetch = srv.Client()

	tasks := &fakeTaskEnqueuer{}
	r := f.routerWithMedia(nil, tasks, resolver)

	msg := f.inbound(t, eventSingleChat, f.sender,
		`{"msgid":"m-img-2","msg_type":"image","images":["`+srv.URL+`/a.png?sig=topsecret"]}`)
	if err := r.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// The durable message lands on the ACK path; media resolution and the
	// bind are detached, so poll rather than sleep — router.Drain is NOT
	// usable as the wait here, it cancels the media context that the bind
	// still needs (the WeCom media-bind DB test's discovery, reused).
	var messageID, sessionID, body string
	var attachments int
	var settled bool
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := f.pool.QueryRow(context.Background(), `
			SELECT m.id::text, m.chat_session_id::text, m.content,
			       (SELECT count(*) FROM attachment a WHERE a.chat_message_id = m.id),
			       m.channel_media_pending_until IS NULL
			FROM chat_message m
			JOIN channel_chat_session_binding b ON b.chat_session_id = m.chat_session_id
			WHERE b.installation_id = $1`, f.installationStr).
			Scan(&messageID, &sessionID, &body, &attachments, &settled)
		if err == nil && attachments == 1 && settled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("media never settled onto the ingested message: err=%v attachments=%d settled=%v",
				err, attachments, settled)
		}
		time.Sleep(100 * time.Millisecond)
	}
	drainRouter(t, r)

	if body != "[图片]" {
		t.Errorf("durable body = %q, want the placeholder", body)
	}
	if strings.Contains(body, "a.png") || strings.Contains(body, "topsecret") {
		t.Errorf("durable body leaks the signed media url: %q", body)
	}

	// The attachment landed on the chat message, pointing at the storage
	// object — not the platform url.
	var attURL, filename, contentType string
	f.fx.QueryRow(t, `
		SELECT url, filename, content_type FROM attachment
		WHERE workspace_id = $1 AND chat_message_id = $2`,
		f.workspaceIDStr, messageID).Scan(&attURL, &filename, &contentType)
	if !strings.HasPrefix(attURL, "https://store.test/workspaces/") {
		t.Errorf("attachment url = %q, want the storage object url", attURL)
	}
	if strings.Contains(attURL, "topsecret") {
		t.Errorf("attachment url carries the platform signature: %q", attURL)
	}
	if filename != "tuitui-image-1.png" || contentType != "image/png" {
		t.Errorf("attachment name/type = %q/%q", filename, contentType)
	}

	// The bind transaction claims (removes) the intent it consumed: commit
	// landed ⇔ intents gone.
	if got := f.fx.Count(t, `
		SELECT count(*) FROM channel_media_pending_object WHERE installation_id = $1`,
		f.installationStr); got != 0 {
		t.Errorf("leftover intent rows = %d, want 0 after a successful bind", got)
	}
	// The media-pending deadline cleared, so the deferred run could promote.
	if got := f.fx.Count(t, `
		SELECT count(*) FROM chat_message WHERE id = $1 AND channel_media_pending_until IS NOT NULL`,
		messageID); got != 0 {
		t.Errorf("media-pending deadline still set after bind: %d row(s)", got)
	}
	if calls := tasks.recorded(); len(calls) != 1 || calls[0].sessionID != mustRouteUUID(t, sessionID) {
		t.Errorf("run enqueues = %+v, want exactly one for session %s", calls, sessionID)
	}
}

// TestTuituiRouterDropsUnaddressedGroupMessage keeps the pre-identity group
// gate honest: an ordinary group post (the bot was not @-mentioned in it,
// whether the platform sent at_me:false or no at_me at all) is dropped and
// audited, and an unbound sender never even reaches the identity check.
func TestTuituiRouterDropsUnaddressedGroupMessage(t *testing.T) {
	for _, tc := range []struct{ name, data string }{
		{"at_me_false", `{"msgid":"m-grp-1","msg_type":"text","text":"casual chatter","group_id":"778899","at_me":false}`},
		{"at_me_absent", `{"msgid":"m-grp-1","msg_type":"text","text":"casual chatter","group_id":"778899"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTuituiRouteFixture(t)
			r := f.router(f.replier(), &fakeTaskEnqueuer{})

			msg := f.inbound(t, eventGroupChat, f.sender, tc.data)
			if msg.AddressedToBot {
				t.Fatal("a group message that does not @ the bot must not be addressed")
			}
			if err := r.Handle(context.Background(), msg); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			drainRouter(t, r)

			var reason string
			f.fx.QueryRow(t, `
		SELECT drop_reason FROM channel_inbound_audit
		WHERE installation_id = $1 AND channel_message_id = $2`,
				f.installationStr, "m-grp-1").Scan(&reason)
			if reason != string(engine.DropReasonNotAddressedInGroup) {
				t.Errorf("drop reason = %q, want %q", reason, engine.DropReasonNotAddressedInGroup)
			}
			if got := f.fx.Count(t, `SELECT count(*) FROM channel_binding_token WHERE installation_id = $1`, f.installationStr); got != 0 {
				t.Errorf("binding tokens = %d, want 0 — idle group chatter must not spam prompts", got)
			}
		})
	}
}

// TestTuituiRouterAddressedGroupBindsGroupSession closes the group loop: a
// message that @-mentions the bot (data.at_me) is addressed, and the binding
// is keyed by the group id with chat_type group. The ref block rides along
// because a real @ of the bot in this platform arrives as a quote of its last
// answer, and the quote must not be what decides this case.
func TestTuituiRouterAddressedGroupBindsGroupSession(t *testing.T) {
	f := newTuituiRouteFixture(t)
	f.bindSender()
	r := f.router(f.replier(), &fakeTaskEnqueuer{})

	msg := f.inbound(t, eventGroupChat, f.sender,
		`{"msgid":"m-grp-2","msg_type":"text","text":"answer this","group_id":"778899","at_me":true,
		  "ref":{"msgid":"bot-prev","content":"earlier answer","user_name":"bot","is_me":true}}`)
	if !msg.AddressedToBot {
		t.Fatal("a group message with at_me true must count as addressed")
	}
	if err := r.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	drainRouter(t, r)

	var chatType, chatID string
	f.fx.QueryRow(t, `
		SELECT chat_type, channel_chat_id FROM channel_chat_session_binding WHERE installation_id = $1`,
		f.installationStr).Scan(&chatType, &chatID)
	if chatType != "group" || chatID != "778899" {
		t.Errorf("group binding = (%q, %q), want (group, 778899)", chatType, chatID)
	}
}

// TestTuituiRouterIssueCommandStampsOrigin is the provenance assertion. The
// issue a fully routed `/issue` command creates — real inbound frame, real
// ResolverSet, real Router, real IssueService, real Postgres — carries
// origin_type='tuitui_chat' plus origin_id=<the chat session it was typed
// in>, the same stamp every other channel writes. Migrations 510/511 widened
// the live issue.origin_type CHECK to admit the label; before that the write
// failed SQLSTATE 23514, so the ResolverSet left OriginType empty and the
// command filed unattributed issues.
func TestTuituiRouterIssueCommandStampsOrigin(t *testing.T) {
	f := newTuituiRouteFixture(t)
	f.bindSender()
	// The issue is written by the production create path, so the Fixture's
	// per-row cleanup cannot reach it. agent_task_queue / comment / label rows
	// hang off it by ON DELETE CASCADE, so this one delete clears the rest.
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`DELETE FROM issue WHERE workspace_id = $1 AND origin_type = $2`,
			f.workspaceID, originTuituiChat)
	})

	tasks := &fakeTaskEnqueuer{}
	r := f.issueRouter(tasks)

	msg := f.inbound(t, eventSingleChat, f.sender,
		`{"msgid":"m-origin-1","msg_type":"text","text":"/issue Fix the login redirect"}`)
	if err := r.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	drainRouter(t, r)

	var sessionID string
	f.fx.QueryRow(t, `
		SELECT chat_session_id::text FROM channel_chat_session_binding WHERE installation_id = $1`,
		f.installationStr).Scan(&sessionID)

	var originCount int
	f.fx.QueryRow(t, `
		SELECT count(*) FROM issue WHERE workspace_id = $1 AND origin_type = $2`,
		f.workspaceIDStr, originTuituiChat).Scan(&originCount)
	if originCount != 1 {
		t.Fatalf("tuitui_chat issues = %d, want exactly 1", originCount)
	}

	var originID, title, creatorType, creatorID string
	f.fx.QueryRow(t, `
		SELECT origin_id::text, title, creator_type, creator_id::text
		FROM issue WHERE workspace_id = $1 AND origin_type = $2`,
		f.workspaceIDStr, originTuituiChat).
		Scan(&originID, &title, &creatorType, &creatorID)
	if originID != sessionID {
		t.Errorf("origin_id = %q, want the session the command was typed in %q", originID, sessionID)
	}
	if title != "Fix the login redirect" {
		t.Errorf("title = %q, want the parsed command title", title)
	}
	if creatorType != "member" || creatorID != f.userIDStr {
		t.Errorf("creator = %s/%s, want member/%s (the sender, not the installer)", creatorType, creatorID, f.userIDStr)
	}

	// A terminal issue command must not also schedule a chat run.
	if calls := tasks.recorded(); len(calls) != 0 {
		t.Errorf("chat run enqueues = %d, want 0 for a terminal /issue command: %+v", len(calls), calls)
	}
}
