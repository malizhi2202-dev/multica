package tuitui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ErrInvalidAppID / ErrInvalidAppSecret are returned by RegisterBYO when a
// pasted credential is empty. The handler maps them to 400 so the dialog can
// show a precise hint instead of a generic failure.
var (
	ErrInvalidAppID     = errors.New("tuitui: app_id is required")
	ErrInvalidAppSecret = errors.New("tuitui: app_secret is required")
)

// RegisterBYOParams are the inputs for a bring-your-own-app install: the agent
// this bot represents, who is installing, and the two credentials the user
// pasted from their own Tuitui robot application.
type RegisterBYOParams struct {
	WorkspaceID pgtype.UUID
	AgentID     pgtype.UUID
	InitiatorID pgtype.UUID
	AppID       string
	AppSecret   string // encrypted at rest; never stored or returned in plaintext
}

// RegisterBYO installs a user-supplied ("bring your own") Tuitui robot for an
// agent. The user creates their own Tuitui application and pastes its app_id +
// app_secret; there is NO OAuth code exchange.
//
// Unlike DingTalk (which proves a pasted pair by minting an access_token) this
// path performs NO live credential probe: Tuitui authenticates every request
// with the appid/secret query pair and exposes no token-mint or read-only
// identity endpoint, so validating at paste time would either send a real
// message to a chat or invent an API the platform does not have. A wrong secret
// therefore surfaces at first connection — the Supervisor logs the failed
// WebSocket dial and keeps retrying — rather than as a 400 here.
//
// Each BYO app is a distinct bot identity, so the SAME Tuitui organization can
// host several of them, one per agent. The stored config carries the app_id as
// the routing key (config->>'app_id'); persistInstall keys the row by
// (workspace, agent), reclaims a DEAD prior owner of that app_id, and refuses a
// LIVE owner with an accurate conflict sentinel. The WebSocket connection that
// consumes these credentials lives in tuitui_channel.go; this method only
// persists the installation.
func (s *InstallService) RegisterBYO(ctx context.Context, p RegisterBYOParams) (db.ChannelInstallation, error) {
	appID := strings.TrimSpace(p.AppID)
	appSecret := strings.TrimSpace(p.AppSecret)
	if appID == "" {
		return db.ChannelInstallation{}, ErrInvalidAppID
	}
	if appSecret == "" {
		return db.ChannelInstallation{}, ErrInvalidAppSecret
	}

	sealedSecret, err := s.box.Seal([]byte(appSecret))
	if err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("encrypt tuitui app secret: %w", err)
	}
	// Host / Port / MaxMessageLength / ReactionEmoji stay unset: FromInstallConfig
	// fills the platform defaults, so the row records only what the user chose.
	cfgJSON, err := json.Marshal(installConfig{
		AppID:              appID,
		AppSecretEncrypted: base64.StdEncoding.EncodeToString(sealedSecret),
	})
	if err != nil {
		return db.ChannelInstallation{}, fmt.Errorf("encode tuitui installation config: %w", err)
	}

	return s.persistInstall(ctx, installPersist{
		wsID:        p.WorkspaceID,
		agentID:     p.AgentID,
		installerID: p.InitiatorID,
		appIDKey:    appID,
		configJSON:  cfgJSON,
	})
}
