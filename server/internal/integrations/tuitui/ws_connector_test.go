package tuitui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// fakeConn implements wsConn over an in-memory frame queue for connector
// tests (no sockets, no network).
type fakeConn struct {
	in        chan []byte
	writes    [][]byte
	closed    bool
	mu        sync.Mutex
	readErr   error
	onReadErr func()
}

func newFakeConn() *fakeConn {
	return &fakeConn{in: make(chan []byte, 16)}
}

func (f *fakeConn) push(frame string) { f.in <- []byte(frame) }

func (f *fakeConn) ReadMessage() (int, []byte, error) {
	data, ok := <-f.in
	if !ok {
		if f.readErr != nil {
			return 0, nil, f.readErr
		}
		return 0, nil, io.ErrClosedPipe
	}
	return websocket.TextMessage, data, nil
}

func (f *fakeConn) WriteMessage(_ int, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, slices.Clone(data))
	return nil
}

func (f *fakeConn) WriteControl(int, []byte, time.Time) error { return nil }
func (f *fakeConn) SetReadDeadline(time.Time) error           { return nil }
func (f *fakeConn) SetPongHandler(func(string) error)         {}
func (f *fakeConn) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.in)
	}
	return nil
}

type fakeDialer struct {
	conn *fakeConn
	url  string
	err  error
}

func (d *fakeDialer) dial(_ context.Context, dialURL string) (wsConn, error) {
	d.url = dialURL
	if d.err != nil {
		return nil, d.err
	}
	return d.conn, nil
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestConnectorAcksAndDispatchesFrames(t *testing.T) {
	conn := newFakeConn()
	dialer := &fakeDialer{conn: conn}

	var deliveries atomic.Int64
	ch := New(baseConfig(), func(ctx context.Context, msg channel.InboundMessage) error {
		deliveries.Add(1)
		if msg.MessageID != "m-1" {
			t.Errorf("handler got msgid %q", msg.MessageID)
		}
		return nil
	}, ConfigDeps{Logger: quietLogger(), Dialer: dialer})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ch.Connect(ctx) }()

	// A chat event, a keepalive, an unnumbered frame, and a redelivery.
	conn.push(`{"event_id":"e1","body":{"event":"single_chat","user_account":"alice","user_name":"A","data":{"msgid":"m-1","msg_type":"text","text":"hi"}}}`)
	conn.push(`{"event_id":"e2","body":{"event":"keepalive"}}`)
	conn.push(`{"body":{"event":"single_chat"}}`) // no event_id → never acked
	conn.push(`{"event_id":"e1","body":{"event":"single_chat","user_account":"alice","data":{"msgid":"m-1"}}}`)

	// Wait until all four frames were read (ack writes for e1 twice? dedup
	// still acks redeliveries), then cancel.
	deadline := time.After(2 * time.Second)
	for {
		conn.mu.Lock()
		n := len(conn.writes)
		conn.mu.Unlock()
		if n == 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("ack writes = %d, want 3: %s", n, conn.writes)
		default:
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Connect after cancel = %v, want nil", err)
	}

	// Every acked frame is answered {"ack": event_id} in delivery order.
	var acks []string
	conn.mu.Lock()
	for _, w := range conn.writes {
		var a ackFrame
		if err := json.Unmarshal(w, &a); err != nil {
			t.Fatalf("decode ack %q: %v", w, err)
		}
		acks = append(acks, a.Ack)
	}
	conn.mu.Unlock()
	if len(acks) != 3 || acks[0] != "e1" || acks[1] != "e2" || acks[2] != "e1" {
		t.Errorf("acks = %v, want [e1 e2 e1] (redelivery acked but deduped)", acks)
	}
	// keepalive is acked without message parsing, the unnumbered frame is
	// ignored outright, and the redelivered e1 is deduped: exactly one
	// delivery reaches the core.
	if got := deliveries.Load(); got != 1 {
		t.Errorf("handler deliveries = %d, want 1", got)
	}
	if got := ch.kindOf("alice"); got != kindP2P {
		t.Errorf("kind map not warmed by inbound: %d", got)
	}
	if !slices.Contains([]string{"wss://im.live.360.cn:8282/robot/callback/ws?auth=app-1.sec-1"}, dialer.url) {
		t.Errorf("dial url = %s", dialer.url)
	}
}

func TestConnectorTeamsModifyDeliveredAndCardsDropped(t *testing.T) {
	conn := newFakeConn()
	calls := make(chan channel.InboundMessage, 4)
	ch := New(baseConfig(), func(ctx context.Context, msg channel.InboundMessage) error {
		calls <- msg
		return nil
	}, ConfigDeps{Logger: quietLogger(), Dialer: &fakeDialer{conn: conn}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ch.Connect(ctx) }()

	conn.push(`{"event_id":"t1","body":{"event":"teams_post_modify","user_account":"bob","data":{"team_id":"t","channel_id":"c","post_id":"p","content":"edited"}}}`)
	conn.push(`{"event_id":"t2","body":{"event":"teams_post_create","user_account":"bob","data":{"team_id":"t","channel_id":"c","post_id":"p2","parent_id":"th","content":"new"}}}`)
	// A card-button callback in its nested shape and its event-name shape:
	// acked, never delivered — the agent must not get a garbage turn.
	conn.push(`{"event_id":"c1","body":{"event":"interactive_action","data":{"message":{"msgid":"p2","action":[{"value":"approve"}],"user":{"account":"bob"}}}}}`)
	conn.push(`{"event_id":"c2","body":{"event":"card_click","data":{"message":{"msgid":"p2","action":{"value":"x"}}}}}`)

	// The two teams posts reach the core…
	seen := map[string]channel.InboundMessage{}
	for _, want := range []string{"p", "p2"} {
		select {
		case msg := <-calls:
			seen[msg.MessageID] = msg
		case <-time.After(2 * time.Second):
			t.Fatalf("teams event %q did not reach handler", want)
		}
	}
	if seen["p"].Source.ChatID != "teams_t_c_p" || seen["p"].Source.ChatType != channel.ChatTypeGroup {
		t.Errorf("modify chat = %+v", seen["p"].Source)
	}
	if seen["p2"].Source.ChatID != "teams_t_c_th" {
		t.Errorf("create chat = %q", seen["p2"].Source.ChatID)
	}
	// …and nothing else arrives.
	select {
	case msg := <-calls:
		t.Fatalf("unexpected delivery %+v", msg)
	case <-time.After(150 * time.Millisecond):
	}
	// All four frames were acked (cards too — the gateway demands acks for
	// every event_id, handled or not).
	conn.mu.Lock()
	nWrites := len(conn.writes)
	conn.mu.Unlock()
	if nWrites != 4 {
		t.Errorf("ack writes = %d, want 4", nWrites)
	}
}

func TestConnectDialFailureReturnsError(t *testing.T) {
	dialer := &fakeDialer{err: errors.New("dial refused")}
	ch := New(baseConfig(), func(ctx context.Context, msg channel.InboundMessage) error { return nil },
		ConfigDeps{Logger: quietLogger(), Dialer: dialer})
	err := ch.Connect(context.Background())
	if err == nil {
		t.Fatal("dial failure must surface to the supervisor")
	}
	// The credential-bearing dial URL must never be echoed in the error.
	if strings.Contains(err.Error(), "sec-1") || strings.Contains(err.Error(), "auth=") {
		t.Errorf("error leaks credentials: %v", err)
	}
}

// Disconnect must end a running Connect with nil (graceful stop), be
// idempotent, and leave the channel re-connectable (supervisor lifecycle).
func TestDisconnectEndsConnectAndReconnects(t *testing.T) {
	dialer := &fakeDialer{conn: newFakeConn()}
	ch := New(baseConfig(), func(ctx context.Context, msg channel.InboundMessage) error { return nil },
		ConfigDeps{Logger: quietLogger(), Dialer: dialer})

	done := make(chan error, 1)
	go func() { done <- ch.Connect(context.Background()) }()

	// Give the session a moment to register, then Disconnect twice.
	time.Sleep(50 * time.Millisecond)
	if err := ch.Disconnect(context.Background()); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if err := ch.Disconnect(context.Background()); err != nil {
		t.Fatalf("second Disconnect: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Connect after Disconnect = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Disconnect did not end Connect")
	}

	// Repeated Connect after the session ended must work: fresh dialer with
	// one frame to prove the loop runs again, then ctx-cancel returns nil.
	conn2 := newFakeConn()
	ch.dialer = &fakeDialer{conn: conn2}
	ctx, cancel := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() { done2 <- ch.Connect(ctx) }()
	conn2.push(`{"event_id":"r1","body":{"event":"single_chat","user_account":"alice","data":{"msgid":"m-2","msg_type":"text","text":"again"}}}`)
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done2; err != nil {
		t.Fatalf("re-Connect after cancel = %v, want nil", err)
	}
}
