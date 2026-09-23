package tuitui

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// tuituiChannel is ONE installation's Tuitui connection: a blocking receive
// loop over the callback WebSocket (Connect), with outbound sends through the
// query-authenticated HTTP robot API. The engine.Supervisor builds one via
// the registered Factory and owns the lease / reconnect lifecycle.
type tuituiChannel struct {
	cfg     Config
	client  *client
	handler channel.InboundHandler
	logger  *slog.Logger

	// dialer overrides the WebSocket dialer (tests inject a fake socket).
	dialer wsDialer

	// kinds remembers the event kind the receive loop observed per plain
	// chat id (the real event_type, per the spec's "remember the true
	// event kind" rule). It is a routing ACCURACY cache, never the teams
	// discriminator: teams is prefix-only (see routeKind) and cold-map
	// plain chats fall back to the platform ID format, so a restart can
	// never misroute a teams conversation into a plain payload.
	mu         sync.Mutex
	kinds      map[string]chatKind
	seenIDs    map[string]struct{}
	seenMru    []string
	connCancel context.CancelFunc // set while a Connect session is running
}

// maxSeenEvents bounds the in-memory event_id dedup set (the reference
// client kept ~2000); redeliveries past the bound fall through to the
// engine's (installation, MessageID) dedup, which is the durable guard.
const maxSeenEvents = 2000

func (c *tuituiChannel) Type() channel.Type { return TypeTuitui }

// Capabilities declares what the Tuitui cloud supports. Honest limits:
//   - Rich cards exist only for p2p/group chats; teams conversations take
//     markdown (Send handles the routing; the interactive methods fail for
//     teams rather than misrender).
//   - No quote-reply on sends: the platform send API has no quote field (the
//     reference client accepts reply_to and ignores it), so CapQuoteReply is
//     NOT declared even though inbound carries refs.
//   - No typing indicator. Reactions and media upload exist but have no
//     Capability bit; they are exposed as channel methods.
func (c *tuituiChannel) Capabilities() channel.Capability {
	return channel.CapText | channel.CapRichCard | channel.CapThreadReply |
		channel.CapMessageEdit | channel.CapAttachment
}

// Connect dials the callback WebSocket and blocks delivering frames into the
// shared InboundHandler until ctx is cancelled (nil), Disconnect is called
// (nil), or the socket breaks (error → the supervisor backs off and redials).
// Tolerates repeated calls: each invocation owns one socket session.
func (c *tuituiChannel) Connect(ctx context.Context) error {
	if c.handler == nil {
		return errors.New("tuitui: inbound handler not configured")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.mu.Lock()
	if c.connCancel != nil {
		c.mu.Unlock()
		return errors.New("tuitui: Connect already running")
	}
	c.connCancel = cancel
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.connCancel = nil
		c.mu.Unlock()
	}()

	conn := &wsConnector{
		dialURL: c.cfg.wsURL(),
		dialer:  c.dialer,
		onFrame: c.handleFrame,
		logger:  c.logger,
	}
	return conn.run(runCtx)
}

// Disconnect cancels a running Connect session (unblocking its read loop and
// closing the socket) and is safe to call more than once or before any
// Connect. The socket itself is owned by the Connect goroutine, so there is
// nothing else to tear down; repeated calls after the session already ended
// are no-ops returning nil.
func (c *tuituiChannel) Disconnect(ctx context.Context) error {
	c.mu.Lock()
	cancel := c.connCancel
	c.connCancel = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// handleFrame processes one acknowledged inbound frame: transient event_id
// dedup, normalization into channel.InboundMessage, kind bookkeeping, then
// the shared handler. Drops are logged; the durable dedup / identity gates
// belong to the engine. The frame has already been acked by the connector.
func (c *tuituiChannel) handleFrame(ctx context.Context, frame *wsFrame) {
	eventID := frame.eventID()
	if eventID != "" && c.seenEvent(eventID) {
		c.logger.DebugContext(ctx, "tuitui: dropped redelivered event", "event_id", eventID)
		return
	}

	// Card-button callbacks ride the same channel as chat events. They are
	// acknowledged (done by the connector) but have no consumer in Multica
	// yet, so they are dropped explicitly here — never as a chat message
	// (that would feed the agent a garbage turn on every button tap).
	if frame.Body.Event == eventInteractiveAction || looksLikeCardCallback(&frame.Body) {
		c.logger.InfoContext(ctx, "tuitui: unhandled interactive card callback dropped",
			"event", frame.Body.Event, "event_id", eventID)
		return
	}

	norm, ok := normalizeEvent(frame)
	if !ok {
		c.logger.InfoContext(ctx, "tuitui: dropped inbound event",
			"event", frame.Body.Event, "event_id", eventID)
		return
	}
	c.rememberKind(norm.msg.Source.ChatID, norm.kind)
	if err := c.handler(ctx, norm.msg); err != nil {
		c.logger.WarnContext(ctx, "tuitui: inbound handler error", "error", err)
	}
}

func (c *tuituiChannel) rememberKind(chatID string, kind chatKind) {
	if chatID == "" || kind == kindUnknown {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.kinds == nil {
		c.kinds = make(map[string]chatKind)
	}
	c.kinds[chatID] = kind
}

func (c *tuituiChannel) kindOf(chatID string) chatKind {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.kinds[chatID]
}

// seenEvent reports whether eventID was already processed, retaining a
// bounded FIFO window like the reference client's trimmed set.
func (c *tuituiChannel) seenEvent(eventID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seenIDs == nil {
		c.seenIDs = make(map[string]struct{})
	}
	if _, dup := c.seenIDs[eventID]; dup {
		return true
	}
	c.seenIDs[eventID] = struct{}{}
	c.seenMru = append(c.seenMru, eventID)
	if len(c.seenMru) > maxSeenEvents {
		for _, evict := range c.seenMru[:len(c.seenMru)-maxSeenEvents] {
			delete(c.seenIDs, evict)
		}
		c.seenMru = c.seenMru[len(c.seenMru)-maxSeenEvents:]
	}
	return false
}

// ChannelDeps are the shared dependencies the Tuitui Factory closes over.
// The engine inbound handler is supplied per-build via channel.Config.Handler.
type ChannelDeps struct {
	Logger *slog.Logger
	// Decrypt turns a stored app_secret_encrypted blob into plaintext, the
	// way the DingTalk adapter's factory consumes secretbox.Open. Nil treats
	// the ciphertext bytes as plaintext (test convenience).
	Decrypt Decrypter
	// HTTPClient overrides the outbound send client (tests). Nil uses a
	// default with a request-scoped timeout.
	HTTPClient *http.Client
	// Dialer overrides the WebSocket dialer (tests). Nil uses gorilla.
	Dialer wsDialer
}

// RegisterTuitui registers the per-installation Tuitui Factory so the
// engine.Supervisor builds + supervises one channel per active
// installation. Same contract as dingtalk.RegisterDingTalk — no engine edit.
func RegisterTuitui(reg *channel.Registry, deps ChannelDeps) {
	reg.Register(TypeTuitui, NewFactory(deps))
}

// NewFactory builds the channel.Factory the registry calls per installation.
// It validates the credential blob up front: missing app_id / app_secret is
// an error, never a half-built connection. Secrets may be stored plaintext
// (app_secret) or secretbox-encrypted (app_secret_encrypted + deps.Decrypt),
// mirroring the DingTalk adapter's in-adapter decryption.
func NewFactory(deps ChannelDeps) channel.Factory {
	return func(cfg channel.Config) (channel.Channel, error) {
		c, err := FromConfigDecrypted(cfg, deps.Decrypt)
		if err != nil {
			return nil, err
		}
		logger := deps.Logger
		if logger == nil {
			logger = slog.Default()
		}
		return New(c, cfg.Handler, ConfigDeps{
			Logger:     logger,
			HTTPClient: deps.HTTPClient,
			Dialer:     deps.Dialer,
		}), nil
	}
}

// ConfigDeps carries the per-channel transport overrides.
type ConfigDeps struct {
	Logger     *slog.Logger
	HTTPClient *http.Client
	Dialer     wsDialer
}

// New assembles a channel from validated credentials — the seam tests use
// to inject the fake dialer / httptest-backed client without JSON blobs.
func New(cfg Config, handler channel.InboundHandler, deps ConfigDeps) *tuituiChannel {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &tuituiChannel{
		cfg:     cfg,
		client:  newClient(cfg, deps.HTTPClient),
		handler: handler,
		logger:  logger,
		dialer:  deps.Dialer,
		kinds:   map[string]chatKind{},
		seenIDs: map[string]struct{}{},
		seenMru: nil,
	}
}

// compile-time contract check.
var _ channel.Channel = (*tuituiChannel)(nil)
