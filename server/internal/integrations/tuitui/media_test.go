package tuitui

// media_test.go — the resolver half of the Tuitui media feature: the
// frame-source matrix lives in inbound_test.go; this file drives
// HasMedia/ResolveMedia through httptest download servers with an injected
// fetch client (the DingTalk resolver's test convention), so the default
// suite never touches the public internet, and pins the degraded outcomes
// (oversize / non-image type / 4xx / expired budget) to the same
// best-effort contract the copied channels keep: a failed attachment is
// logged, leaves its intent row for the reconciler, uploads nothing, adds
// no MediaRef, and never rewrites the message text.

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
)

var mediaTestPNG = append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, bytes.Repeat([]byte{0x7F}, 48)...)

// orderedLog lets a test prove the intent row is written BEFORE the upload
// without racing the recorder itself.
type orderedLog struct {
	mu    sync.Mutex
	items []string
}

func (l *orderedLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.items = append(l.items, s)
}

func (l *orderedLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.items...)
}

type fakeMediaStore struct {
	log     *orderedLog
	mu      sync.Mutex
	uploads map[string]fakeUpload
}

type fakeUpload struct {
	data        []byte
	contentType string
	filename    string
}

func newFakeMediaStore() *fakeMediaStore {
	return &fakeMediaStore{log: &orderedLog{}, uploads: map[string]fakeUpload{}}
}

func (f *fakeMediaStore) Upload(_ context.Context, key string, data []byte, contentType, filename string) (string, error) {
	f.log.add("upload:" + key)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads[key] = fakeUpload{data: append([]byte(nil), data...), contentType: contentType, filename: filename}
	return f.ObjectURL(key), nil
}

func (f *fakeMediaStore) ObjectURL(key string) string { return "https://store.test/" + key }

func (f *fakeMediaStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.uploads)
}

type fakeIntentLedger struct {
	log   *orderedLog
	mu    sync.Mutex
	calls []engine.RecordPendingMediaObjectParams
	// refuse mimics the reconciler owning the key (ok=false).
	refuse bool
}

func (l *fakeIntentLedger) RecordPendingMediaObject(_ context.Context, p engine.RecordPendingMediaObjectParams) (bool, error) {
	l.log.add("intent:" + p.StorageKey)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, p)
	return !l.refuse, nil
}

func (l *fakeIntentLedger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.calls)
}

func mediaTestInstallation() engine.ResolvedInstallation {
	var ws, inst pgtype.UUID
	ws.Bytes[0], inst.Bytes[0] = 0xAB, 0xCD
	ws.Valid, inst.Valid = true, true
	return engine.ResolvedInstallation{ID: inst, WorkspaceID: ws}
}

func mediaTestMessageID() pgtype.UUID {
	var id pgtype.UUID
	id.Bytes[0] = 0xEF
	id.Valid = true
	return id
}

// mediaTestMsg normalizes a real frame so the resolver consumes exactly the
// Raw envelope production writes.
func mediaTestMsg(t *testing.T, event, data string) channel.InboundMessage {
	t.Helper()
	n, ok := normalizeEvent(frameJSON(t, event, data), "app-1")
	if !ok {
		t.Fatalf("test frame did not normalize: %s", data)
	}
	return n.msg
}

func newMediaTestResolver(t *testing.T, store *fakeMediaStore, ledger *fakeIntentLedger, fetch *http.Client, maxBytes int64) *mediaResolver {
	t.Helper()
	r := NewMediaResolver(store, ledger, testDiscardLogger()).(*mediaResolver)
	if fetch != nil {
		r.fetch = fetch
	}
	if maxBytes > 0 {
		r.maxBytes = maxBytes
	}
	return r
}

func TestTuituiMediaHasMediaIsPureFrameDecode(t *testing.T) {
	store := newFakeMediaStore()
	ledger := &fakeIntentLedger{log: store.log}
	r := newMediaTestResolver(t, store, ledger, nil, 0)

	if r.HasMedia(mediaTestMsg(t, eventSingleChat, `{"msgid":"m","msg_type":"text","text":"hi"}`)) {
		t.Error("text message must not schedule media work")
	}
	if !r.HasMedia(mediaTestMsg(t, eventSingleChat, `{"msgid":"m","msg_type":"image","images":["https://media.test/a.png"]}`)) {
		t.Error("image message must schedule media work")
	}
	if r.HasMedia(mediaTestMsg(t, eventSingleChat, `{"msgid":"m","msg_type":"image","images":[{"name":"urlless"}]}`)) {
		t.Error("entries without a url are not media work")
	}
	if !r.HasMedia(mediaTestMsg(t, eventTeamsPostCreate,
		`{"team_id":"t","channel_id":"c","post_id":"p","content":"x","files":[{"url":"https://media.test/f"}]}`)) {
		t.Error("teams files are media work")
	}
	if r.HasMedia(channel.InboundMessage{}) {
		t.Error("undecodable Raw must report no media, not panic")
	}
	if store.count() != 0 || ledger.count() != 0 {
		t.Error("HasMedia performed I/O side effects")
	}
}

func TestTuituiMediaResolveHappyPathImagesAndFiles(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(mediaTestPNG)
		case "/doc.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = io.WriteString(w, "%PDF-1.4\n")
		default:
			http.Error(w, "gone", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	store := newFakeMediaStore()
	ledger := &fakeIntentLedger{log: store.log}
	r := newMediaTestResolver(t, store, ledger, srv.Client(), 0)
	inst, messageID := mediaTestInstallation(), mediaTestMessageID()
	msg := mediaTestMsg(t, eventTeamsPostCreate,
		`{"team_id":"t","channel_id":"c","post_id":"p","content":"look",`+
			`"images":[{"url":"`+srv.URL+`/a.png?sig=topsecret"}],`+
			`"files":[{"url":"`+srv.URL+`/doc.pdf","name":"Quarterly Report.pdf"}]}`)
	if !r.HasMedia(msg) {
		t.Fatal("HasMedia=false for a post with two attachments")
	}

	got := r.ResolveMedia(context.Background(), inst, engine.ResolvedIdentity{}, pgtype.UUID{}, messageID, msg)
	if len(got.MediaRefs) != 2 {
		t.Fatalf("media refs = %+v, want the image and the file", got.MediaRefs)
	}
	image, file := got.MediaRefs[0], got.MediaRefs[1]
	if image.Type != channel.MsgTypeImage || image.MimeType != "image/png" || image.SizeBytes != int64(len(mediaTestPNG)) {
		t.Errorf("image ref = %+v", image)
	}
	if image.Filename != "tuitui-image-1.png" {
		t.Errorf("generated image name = %q, want tuitui-image-1.png", image.Filename)
	}
	if file.Type != channel.MsgTypeFile || file.MimeType != "application/pdf" || file.Filename != "Quarterly Report.pdf" {
		t.Errorf("file ref = %+v", file)
	}
	// Standalone attachments, exactly like lark / wecom / slack: the durable
	// body keeps its placeholder and the InlinePlaceholder field stays empty.
	if image.InlinePlaceholder != "" || file.InlinePlaceholder != "" {
		t.Errorf("refs carry inline placeholders: %+v", got.MediaRefs)
	}
	if got.Text != "look\n[图片]\n[文件] Quarterly Report.pdf" {
		t.Errorf("Text was rewritten: %q", got.Text)
	}
	keyPrefix := "workspaces/" + util.UUIDToString(inst.WorkspaceID) + "/tuitui/" + util.UUIDToString(inst.ID) + "/"
	for i, ref := range got.MediaRefs {
		if !strings.HasPrefix(ref.StorageKey, keyPrefix) {
			t.Errorf("ref %d key = %q, want prefix %q", i, ref.StorageKey, keyPrefix)
		}
		if ref.StorageURL != store.ObjectURL(ref.StorageKey) {
			t.Errorf("ref %d StorageURL = %q, want ObjectURL(key) = %q", i, ref.StorageURL, store.ObjectURL(ref.StorageKey))
		}
		if strings.Contains(ref.StorageKey, "topsecret") || strings.Contains(ref.StorageURL, "sig=") {
			t.Errorf("signed query leaked into object identity: %+v", ref)
		}
	}
	// Every object has its intent, recorded BEFORE the corresponding upload,
	// carrying the same durable coordinates the bind will claim.
	events := store.log.all()
	if len(events) != 4 {
		t.Fatalf("side-effect order = %v, want intent/upload per attachment", events)
	}
	for i := 0; i < 2; i++ {
		if !strings.HasPrefix(events[2*i], "intent:") || !strings.HasPrefix(events[2*i+1], "upload:") {
			t.Fatalf("intent row %d did not precede its upload: %v", i, events)
		}
	}
	if ledger.count() != 2 {
		t.Fatalf("ledger calls = %d, want 2", ledger.count())
	}
	for _, rec := range ledger.calls {
		if rec.ChatMessageID != messageID || rec.InstallationID != inst.ID || rec.WorkspaceID != inst.WorkspaceID {
			t.Errorf("intent row coordinates = %+v", rec)
		}
		if rec.StorageURL != store.ObjectURL(rec.StorageKey) {
			t.Errorf("intent row URL is not the object URL: %+v", rec)
		}
	}
	if store.count() != 2 {
		t.Errorf("uploads = %d, want 2", store.count())
	}
}

func TestTuituiMediaOversizeDegradesLikeTheCopiedChannels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(bytes.Repeat([]byte{0x41}, 256))
	}))
	t.Cleanup(srv.Close)

	store, ledger := newFakeMediaStore(), &fakeIntentLedger{log: &orderedLog{}}
	// A 128-byte ceiling: proving the real cap means shipping 20 MiB bodies,
	// which is the WeCom download helper's documented reason for a parameter.
	r := newMediaTestResolver(t, store, ledger, srv.Client(), 128)
	msg := mediaTestMsg(t, eventSingleChat, `{"msgid":"m","msg_type":"image","images":["`+srv.URL+`/big.png"]}`)

	got := r.ResolveMedia(context.Background(), mediaTestInstallation(), engine.ResolvedIdentity{}, pgtype.UUID{}, mediaTestMessageID(), msg)
	if len(got.MediaRefs) != 0 {
		t.Fatalf("oversize attachment produced refs: %+v", got.MediaRefs)
	}
	// The copied contract: the intent row still goes first (the reconciler
	// must know about the key even if nothing uploaded), no upload happens,
	// and the placeholder text survives untouched.
	if ledger.count() != 1 || store.count() != 0 {
		t.Errorf("intent/upload = %d/%d, want 1/0", ledger.count(), store.count())
	}
	if got.Text != "[图片]" {
		t.Errorf("Text = %q, want the placeholder intact", got.Text)
	}
}

func TestTuituiMediaDeclaredOversizeRefusedBeforeRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "999999")
		// The body lie never gets far: the Content-Length pre-check refuses.
		time.Sleep(50 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	store, ledger := newFakeMediaStore(), &fakeIntentLedger{log: &orderedLog{}}
	r := newMediaTestResolver(t, store, ledger, srv.Client(), 128)
	msg := mediaTestMsg(t, eventSingleChat, `{"msgid":"m","msg_type":"image","images":["`+srv.URL+`/huge.png"]}`)
	if got := r.ResolveMedia(context.Background(), mediaTestInstallation(), engine.ResolvedIdentity{}, pgtype.UUID{}, mediaTestMessageID(), msg); len(got.MediaRefs) != 0 {
		t.Fatalf("declared oversize accepted: %+v", got.MediaRefs)
	}
	if store.count() != 0 {
		t.Errorf("uploads = %d, want none", store.count())
	}
}

func TestTuituiMediaNonImageContentTypeIsStoredNotRejected(t *testing.T) {
	// Tuitui frames carry real documents (teams files, file messages), so —
	// like the WeCom resolver it mirrors — the type is recorded, not
	// policed: dingtalk's image-only allow list would drop every file this
	// platform delivers.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write([]byte("PK\x03\x04junkjunk"))
	}))
	t.Cleanup(srv.Close)

	store, ledger := newFakeMediaStore(), &fakeIntentLedger{log: &orderedLog{}}
	r := newMediaTestResolver(t, store, ledger, srv.Client(), 0)
	msg := mediaTestMsg(t, eventSingleChat, `{"msgid":"m","msg_type":"file","file":{"url":"`+srv.URL+`/bundle.zip","name":"bundle.zip"}}`)
	got := r.ResolveMedia(context.Background(), mediaTestInstallation(), engine.ResolvedIdentity{}, pgtype.UUID{}, mediaTestMessageID(), msg)
	if len(got.MediaRefs) != 1 {
		t.Fatalf("refs = %+v, want the zip stored as the file the frame declared", got.MediaRefs)
	}
	ref := got.MediaRefs[0]
	if ref.Type != channel.MsgTypeFile || ref.MimeType != "application/zip" || ref.Filename != "bundle.zip" {
		t.Errorf("ref = %+v", ref)
	}
}

func TestTuituiMedia4xxDegradesWithoutLeakingTheSignedURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The shape a lapsed signature answers with.
		http.Error(w, "AccessDenied: signature expired", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	var logs bytes.Buffer
	store := newFakeMediaStore()
	ledger := &fakeIntentLedger{log: &orderedLog{}}
	r := newMediaTestResolver(t, store, ledger, srv.Client(), 0)
	r.logger = slog.New(slog.NewTextHandler(&logs, nil))
	msg := mediaTestMsg(t, eventSingleChat,
		`{"msgid":"m-403","msg_type":"image","images":["`+srv.URL+`/a.png?sig=topsecret-credential"]}`)

	got := r.ResolveMedia(context.Background(), mediaTestInstallation(), engine.ResolvedIdentity{}, pgtype.UUID{}, mediaTestMessageID(), msg)
	if len(got.MediaRefs) != 0 {
		t.Fatalf("403 produced refs: %+v", got.MediaRefs)
	}
	if ledger.count() != 1 || store.count() != 0 {
		t.Errorf("intent/upload = %d/%d, want 1/0 (row left for the reconciler)", ledger.count(), store.count())
	}
	line := logs.String()
	if !strings.Contains(line, "http 403") || !strings.Contains(line, "m-403") {
		t.Errorf("failure log = %q, want status + message id", line)
	}
	if strings.Contains(line, "topsecret-credential") {
		t.Errorf("the signed url credential reached the log: %q", line)
	}
}

func TestTuituiMediaExpiredBudgetDegradesQuietly(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	store, ledger := newFakeMediaStore(), &fakeIntentLedger{log: &orderedLog{}}
	r := newMediaTestResolver(t, store, ledger, srv.Client(), 0)
	msg := mediaTestMsg(t, eventSingleChat, `{"msgid":"m","msg_type":"image","images":["`+srv.URL+`/slow.png"]}`)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	got := r.ResolveMedia(ctx, mediaTestInstallation(), engine.ResolvedIdentity{}, pgtype.UUID{}, mediaTestMessageID(), msg)
	if len(got.MediaRefs) != 0 {
		t.Fatalf("stalled server produced refs: %+v", got.MediaRefs)
	}
	if store.count() != 0 {
		t.Errorf("uploads = %d, want none", store.count())
	}
}

func TestTuituiMediaCountCapKeepsTheFirstAttachments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(mediaTestPNG)
	}))
	t.Cleanup(srv.Close)

	images := make([]string, 0, maxAttachmentsPerMessage+2)
	for i := 0; i < maxAttachmentsPerMessage+2; i++ {
		images = append(images, `"`+srv.URL+`/i`+string(rune('a'+i))+`.png"`)
	}
	store, ledger := newFakeMediaStore(), &fakeIntentLedger{log: &orderedLog{}}
	r := newMediaTestResolver(t, store, ledger, srv.Client(), 0)
	msg := mediaTestMsg(t, eventSingleChat,
		`{"msgid":"m","msg_type":"image","images":[`+strings.Join(images, ",")+`]}`)

	got := r.ResolveMedia(context.Background(), mediaTestInstallation(), engine.ResolvedIdentity{}, pgtype.UUID{}, mediaTestMessageID(), msg)
	if len(got.MediaRefs) != maxAttachmentsPerMessage || ledger.count() != maxAttachmentsPerMessage {
		t.Fatalf("refs/intents = %d/%d, want the cap %d", len(got.MediaRefs), ledger.count(), maxAttachmentsPerMessage)
	}
}

func TestTuituiMediaReconcilerOwnedKeyUploadsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(mediaTestPNG)
	}))
	t.Cleanup(srv.Close)

	store := newFakeMediaStore()
	ledger := &fakeIntentLedger{log: &orderedLog{}, refuse: true}
	r := newMediaTestResolver(t, store, ledger, srv.Client(), 0)
	msg := mediaTestMsg(t, eventSingleChat, `{"msgid":"m","msg_type":"image","images":["`+srv.URL+`/a.png"]}`)
	if got := r.ResolveMedia(context.Background(), mediaTestInstallation(), engine.ResolvedIdentity{}, pgtype.UUID{}, mediaTestMessageID(), msg); len(got.MediaRefs) != 0 {
		t.Fatalf("refs = %+v, want none for a key the reconciler owns", got.MediaRefs)
	}
	if store.count() != 0 {
		t.Errorf("uploads = %d, want 0: a resurrected object is exactly what the ledger contract forbids", store.count())
	}
}

// TestTuituiMediaProductionClientRefusesLoopback pins the SSRF posture the
// DingTalk resolver ships: the default fetch client never dials a
// non-public address, so a frame pointing at the metadata endpoint or the
// deployment's own localhost is refused before connect. (The httptest
// tests above inject their own client, which is why the suite needs no
// network.)
func TestTuituiMediaProductionClientRefusesLoopback(t *testing.T) {
	r := NewMediaResolver(nil, nil, testDiscardLogger()).(*mediaResolver)
	for _, target := range []string{"http://127.0.0.1:8080/a.png", "http://localhost:9/x", "http://[::1]/x"} {
		if _, _, err := r.download(context.Background(), target); err == nil {
			t.Errorf("download(%s) succeeded, want the non-public-address refusal", target)
		} else if !strings.Contains(err.Error(), "blocked non-public download target") &&
			!strings.Contains(err.Error(), "resolve download target failed") {
			// localhost may resolve via /etc/hosts; either guard answer is
			// a refusal, anything else is a hole.
			t.Errorf("download(%s) err = %v, want the address guard", target, err)
		}
	}
	if _, _, err := r.download(context.Background(), "file:///etc/passwd"); err == nil ||
		!strings.Contains(err.Error(), "scheme") {
		t.Errorf("file:// err = %v, want a scheme refusal", err)
	}
}
