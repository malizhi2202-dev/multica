package tuitui

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// The Tuitui inbound link is a plain WebSocket the platform pushes callback
// frames down: {"event_id": ..., "body": {...}}. Every frame with an event_id
// must be answered with {"ack": event_id} or the gateway redelivers it. This
// file owns exactly one socket session; reconnect / backoff / the
// per-installation lease live in the shared engine.Supervisor, matching the
// DingTalk Stream connector's division of labor.

const (
	wsPingInterval = 20 * time.Second
	wsReadDeadline = 60 * time.Second
	wsWriteTimeout = 10 * time.Second
)

// wsConn is the slice of *websocket.Conn the connector uses, extracted so
// tests can inject a fake socket.
type wsConn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	WriteControl(messageType int, data []byte, deadline time.Time) error
	SetReadDeadline(t time.Time) error
	SetPongHandler(h func(appData string) error)
	Close() error
}

// wsDialer opens a wsConn for the dial URL. Production wraps gorilla; tests
// inject a fake.
type wsDialer interface {
	dial(ctx context.Context, dialURL string) (wsConn, error)
}

type gorillaDialer struct{}

func (gorillaDialer) dial(ctx context.Context, dialURL string) (wsConn, error) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, dialURL, nil)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// wsConnector runs one installation's receive session. onFrame is called for
// every successfully decoded frame AFTER its ack has been written, so a slow
// or failing core never stalls the gateway's delivery pipeline.
type wsConnector struct {
	dialURL string
	dialer  wsDialer
	onFrame func(ctx context.Context, frame *wsFrame)
	logger  *slog.Logger

	pingInterval time.Duration
	readDeadline time.Duration
	writeTimeout time.Duration
}

func (c *wsConnector) withDefaults() {
	if c.dialer == nil {
		c.dialer = gorillaDialer{}
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	if c.pingInterval == 0 {
		c.pingInterval = wsPingInterval
	}
	if c.readDeadline == 0 {
		c.readDeadline = wsReadDeadline
	}
	if c.writeTimeout == 0 {
		c.writeTimeout = wsWriteTimeout
	}
}

// run dials and services frames until ctx is cancelled (returns nil) or the
// socket breaks (returns the error so the supervisor reconnects under
// backoff).
func (c *wsConnector) run(ctx context.Context) error {
	c.withDefaults()

	conn, err := c.dialer.dial(ctx, c.dialURL)
	if err != nil {
		// The dial URL embeds the auth credential; never echo it back.
		return fmt.Errorf("tuitui ws: dial failed (%T)", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var closeOnce sync.Once
	closeConn := func() { closeOnce.Do(func() { _ = conn.Close() }) }

	// gorilla reads do not observe ctx; closing the socket is what unblocks a
	// read stuck in the syscall when the run context is cancelled.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-runCtx.Done():
			closeConn()
		case <-done:
		}
	}()

	var writeMu sync.Mutex
	write := func(messageType int, data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(messageType, data)
	}

	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		ticker := time.NewTicker(c.pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				writeMu.Lock()
				err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(c.writeTimeout))
				writeMu.Unlock()
				if err != nil {
					return // the read loop owns teardown
				}
			}
		}
	}()

	defer func() {
		closeConn()
		<-pingDone
	}()

	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(c.readDeadline)) })

	for {
		if err := conn.SetReadDeadline(time.Now().Add(c.readDeadline)); err != nil {
			return fmt.Errorf("tuitui ws: set read deadline: %w", err)
		}
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			if runCtx.Err() != nil {
				return nil // ctx cancelled → graceful stop
			}
			return fmt.Errorf("tuitui ws: read: %w", err)
		}
		if msgType != websocket.TextMessage && msgType != websocket.BinaryMessage {
			continue
		}
		var frame wsFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			c.logger.WarnContext(runCtx, "tuitui ws: malformed frame", "error", err)
			continue
		}
		eventID := frame.eventID()
		if eventID == "" {
			// Unnumbered frames are never acknowledged (reference client:
			// no event_id → ignore outright).
			continue
		}
		if err := write(websocket.TextMessage, mustJSON(ackFrame{Ack: eventID})); err != nil {
			if runCtx.Err() != nil {
				return nil
			}
			return fmt.Errorf("tuitui ws: write ack: %w", err)
		}
		if frame.Body.Event == eventKeepalive {
			continue
		}
		c.onFrame(runCtx, &frame)
	}
}

// ackFrame is the acknowledgement the gateway expects for every event_id.
type ackFrame struct {
	Ack string `json:"ack"`
}
