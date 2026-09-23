package tuitui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// This file is the outbound half of the chat round trip: it delivers an
// agent's reply (or failure notice) back into the Tuitui conversation that
// triggered the run — the same job dingtalk.Outbound / slack.Outbound /
// telegram.Outbound do. Without it the Router would ingest the message and
// start the agent, but the answer would only ever appear in the web app.
//
// It answers from the durable channel_task_delivery row the task service
// froze at enqueue time (binding id, chat id, chat type, thread), so a route
// rotation (/new) after the task started cannot make the reply answer into
// the wrong conversation. Sessions with no Tuitui delivery are ignored, so
// it coexists with the other channels' subscribers on the shared bus.

// outboundQueries is the slice of generated queries the subscriber needs.
// *db.Queries satisfies it.
type outboundQueries interface {
	GetChannelTaskDelivery(ctx context.Context, taskID pgtype.UUID) (db.ChannelTaskDelivery, error)
	GetAgentTask(ctx context.Context, id pgtype.UUID) (db.AgentTaskQueue, error)
	TaskHasChannelIngestedMessages(ctx context.Context, taskID pgtype.UUID) (bool, error)
	GetChannelInstallation(ctx context.Context, arg db.GetChannelInstallationParams) (db.ChannelInstallation, error)
}

// Outbound subscribes to the chat-task terminal events and posts Tuitui
// replies through the per-installation robot API.
type Outbound struct {
	q       outboundQueries
	decrypt Decrypter
	// httpClient overrides the send transport (tests inject an httptest
	// client). Nil uses the default with a request-scoped timeout.
	httpClient *http.Client
	logger     *slog.Logger
}

// NewOutbound builds the outbound subscriber over the generated queries and
// the app_secret decrypter.
func NewOutbound(q outboundQueries, decrypt Decrypter, logger *slog.Logger) *Outbound {
	if logger == nil {
		logger = slog.Default()
	}
	return &Outbound{q: q, decrypt: decrypt, logger: logger}
}

// Register subscribes to the terminal chat-task events. A cancelled task
// carries no reply content, so unlike DingTalk (which also clears message
// reactions) there is nothing for it to deliver and it stays unsubscribed.
func (o *Outbound) Register(bus *events.Bus) {
	bus.Subscribe(protocol.EventChatDone, o.handleEvent)
	bus.Subscribe(protocol.EventTaskFailed, o.handleEvent)
}

func (o *Outbound) handleEvent(e events.Event) {
	// Bus delivery is synchronous, so a stuck Tuitui HTTP call must not
	// wedge the publish call site: use a fresh ctx with a tight timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := o.processEvent(ctx, e); err != nil {
		o.logger.WarnContext(ctx, "tuitui outbound: reply delivery failed",
			"error", err, "chat_session_id", e.ChatSessionID)
	}
}

func (o *Outbound) processEvent(ctx context.Context, e events.Event) error {
	taskID, sessionID, ok := taskAndSessionFromEvent(e)
	if !ok || !sessionID.Valid {
		// Issue / autopilot tasks carry no chat_session.
		return nil
	}
	content := eventContent(e)
	if content == "" {
		return nil
	}
	delivery, err := o.q.GetChannelTaskDelivery(ctx, taskID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("lookup tuitui task delivery: %w", err)
	}
	if delivery.ChannelType != string(TypeTuitui) {
		return nil
	}
	task, err := o.q.GetAgentTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("load agent task: %w", err)
	}
	deliver, err := engine.TaskInputIsChannelIngested(ctx, o.q, task)
	if err != nil {
		return fmt.Errorf("classify task input origin: %w", err)
	}
	if !deliver {
		return nil
	}
	inst, err := o.q.GetChannelInstallation(ctx, db.GetChannelInstallationParams{
		ID: delivery.InstallationID, ChannelType: string(TypeTuitui),
	})
	if err != nil {
		return fmt.Errorf("load tuitui installation: %w", err)
	}
	if inst.Status != "active" {
		return nil // revoked between trigger and reply
	}
	// The effective host/port come from the stored installation config —
	// FromConfigDecrypted is the single decode path, applying the adapter
	// defaults only for keys the row omits.
	cfg, err := FromConfigDecrypted(channelConfig{Type: TypeTuitui, Raw: inst.Config}, o.decrypt)
	if err != nil {
		return fmt.Errorf("decode tuitui credentials: %w", err)
	}
	c := newClient(cfg, o.httpClient)
	target := sendTarget{
		kind:     kindForSource(delivery.ChatType, delivery.ChannelChatID),
		chatID:   delivery.ChannelChatID,
		threadID: delivery.ChannelThreadID.String,
	}
	for _, chunk := range splitChunks(content, cfg.MaxMessageLength) {
		payload, err := c.sendPayloadFor(target, chunk)
		if err != nil {
			return err
		}
		if _, err := c.postJSON(ctx, pathSend, payload); err != nil {
			return fmt.Errorf("post tuitui reply: %w", err)
		}
	}
	return nil
}

// eventRetryPending reports whether a task:failed payload belongs to an
// automatic retry that has not run yet. The retry attempt reports its own
// outcome, so the subscriber stays silent.
func eventRetryPending(e events.Event) bool {
	if e.Type != protocol.EventTaskFailed {
		return false
	}
	p, ok := e.Payload.(map[string]any)
	if !ok {
		return false
	}
	retryPending, _ := p["retry_pending"].(bool)
	return retryPending
}

// eventContent extracts the deliverable text from an EventChatDone payload
// (typed, or its map form after a serialization round trip) or an
// EventTaskFailed payload. Empty means stay silent.
//
// For task-failed the text mirrors the web transcript's failure chat_message:
// the broadcast's `error` field carries the same redacted failure text and is
// omitted while an auto-retry is pending, so error-present means deliverable.
func eventContent(e events.Event) string {
	switch p := e.Payload.(type) {
	case protocol.ChatDonePayload:
		return p.Content
	case map[string]any:
		if e.Type == protocol.EventTaskFailed {
			if retryPending, _ := p["retry_pending"].(bool); retryPending {
				return ""
			}
			if s, _ := p["error"].(string); s != "" {
				return "⚠️ " + s
			}
			return ""
		}
		if s, ok := p["content"].(string); ok {
			return s
		}
	}
	return ""
}

func taskAndSessionFromEvent(e events.Event) (taskID, sessionID pgtype.UUID, ok bool) {
	if e.TaskID != "" {
		_ = taskID.Scan(e.TaskID)
	}
	if e.ChatSessionID != "" {
		_ = sessionID.Scan(e.ChatSessionID)
	}
	switch p := e.Payload.(type) {
	case protocol.ChatDonePayload:
		if !taskID.Valid {
			_ = taskID.Scan(p.TaskID)
		}
		if !sessionID.Valid {
			_ = sessionID.Scan(p.ChatSessionID)
		}
	case map[string]any:
		if !taskID.Valid {
			if raw, _ := p["task_id"].(string); raw != "" {
				_ = taskID.Scan(raw)
			}
		}
		if !sessionID.Valid {
			if raw, _ := p["chat_session_id"].(string); raw != "" {
				_ = sessionID.Scan(raw)
			}
		}
	}
	return taskID, sessionID, taskID.Valid
}
