// Package tuitui is the Tuitui (推推) IM integration for the channel-agnostic
// engine. It uses the bring-your-own-app (BYO) model like DingTalk: an agent
// owner creates a Tuitui robot application, and Multica stores its app_id /
// app_secret per channel_installation. Each installation runs its own
// inbound WebSocket long connection (wss://{host}:{port}/robot/callback/ws?
// auth={app_id}.{app_secret}) supervised by the shared engine, and replies
// over the HTTP robot API (https://{host}:{port}/robot/message/custom/send,
// query-string authenticated with appid/secret — NOT headers).
//
// Platform facts this adapter encodes (source: the Python reference client
// tui_coding_agent_bridge/bridge/tuitui_client.py):
//
//   - Every JSON response carries an errcode; non-zero is a failure.
//   - Inbound event types are single_chat, group_chat, teams_post_create and
//     teams_post_modify. Teams (channel) conversations have no platform chat
//     id — the adapter composes "teams_{team}_{channel}[_{thread}]" and maps
//     it onto the closed ChatType set as ChatTypeGroup (the engine has no
//     "channel" chat type).
//   - Teams conversations do NOT support interactive cards; only markdown.
//   - Long messages must be chunked by the sender (default cap 20000 runes).
//   - Success responses return msgid or msgids[]; both shapes are accepted.
package tuitui

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// channelConfig aliases the shared factory input so FromConfig reads as the
// adapter's own entry point.
type channelConfig = channel.Config

// TypeTuitui is the channel discriminator for the Tuitui adapter. Defined
// here (not in the channel core) so registering the platform never edits the
// core, mirroring dingtalk.TypeDingTalk / telegram.TypeTelegram.
const TypeTuitui channel.Type = "tuitui"

// Defaults for the Tuitui cloud endpoints. The host is overridable per
// installation; the port applies to both the HTTPS API and the WSS callback.
const (
	DefaultHost = "im.live.360.cn"
	DefaultPort = 8282

	// DefaultMaxMessageLength is the platform-visible chunk cap for one
	// outbound message (reference config max_message_length: 20000).
	DefaultMaxMessageLength = 20000

	// DefaultReactionEmoji is the acknowledgement reaction text the
	// reference bridge uses ("received" in Chinese).
	DefaultReactionEmoji = "收到"
)

// Config is the validated, per-installation credential set the transport
// runs on. It is decoded from channel.Config.Raw by FromConfig; the wiring
// layer (router) is responsible for decrypting any secretbox-wrapped storage
// form before the Factory sees it, mirroring the DingTalk handler-layer
// convention.
type Config struct {
	// AppID / AppSecret authenticate both transports: the WSS URL embeds
	// "auth={app_id}.{app_secret}", every HTTP call sends query params
	// appid / secret.
	AppID     string
	AppSecret string

	// Host / Port locate the Tuitui cloud. Empty Host uses DefaultHost;
	// zero Port uses DefaultPort.
	Host string
	Port int

	// MaxMessageLength bounds one outbound message before chunking. Zero
	// uses DefaultMaxMessageLength.
	MaxMessageLength int

	// ReactionEmoji is used by SendReaction. Empty uses
	// DefaultReactionEmoji.
	ReactionEmoji string
}

// installConfig is the JSON shape stored in channel_installation.config for
// a Tuitui installation. app_id doubles as the generic routing key
// (config->>'app_id'), matching the DingTalk / Telegram convention.
//
// The secret arrives either as plaintext app_secret (the wiring task's
// credential shape) or as base64 secretbox ciphertext in
// app_secret_encrypted, exactly like dingtalk.installConfig — a nil
// Decrypter treats the decoded bytes as plaintext, the DingTalk test
// convention. Plaintext wins when both are present.
type installConfig struct {
	AppID              string `json:"app_id"`
	AppSecret          string `json:"app_secret"`
	AppSecretEncrypted string `json:"app_secret_encrypted"`
	Host               string `json:"host,omitempty"`
	Port               int    `json:"port,omitempty"`
	MaxMessageLength   int    `json:"max_message_length,omitempty"`
	ReactionEmoji      string `json:"reaction_emoji,omitempty"`
}

// Decrypter turns stored secretbox ciphertext into plaintext (same shape as
// dingtalk.Decrypter / secretbox.Open).
type Decrypter func(ciphertext []byte) (plaintext []byte, err error)

// FromConfig decodes and validates a channel.Config.Raw blob into a Config.
// Missing credentials are an error, never a half-built connection.
func FromConfig(cfg channelConfig) (Config, error) {
	return FromConfigDecrypted(cfg, nil)
}

// FromConfigDecrypted is FromConfig with the secretbox Decrypter the wiring
// layer injects (nil treats ciphertext as plaintext).
func FromConfigDecrypted(cfg channelConfig, decrypt Decrypter) (Config, error) {
	if len(cfg.Raw) == 0 {
		return Config{}, errors.New("tuitui: empty installation config")
	}
	var ic installConfig
	if err := json.Unmarshal(cfg.Raw, &ic); err != nil {
		return Config{}, fmt.Errorf("tuitui: decode installation config: %w", err)
	}
	return FromInstallConfig(ic, decrypt)
}

// FromInstallConfig validates a decoded installConfig. Split out so tests
// and the wiring layer can build a Config without hand-crafting JSON.
func FromInstallConfig(ic installConfig, decrypt Decrypter) (Config, error) {
	secret := strings.TrimSpace(ic.AppSecret)
	if secret == "" && ic.AppSecretEncrypted != "" {
		plain, err := decryptToken(ic.AppSecretEncrypted, decrypt)
		if err != nil {
			return Config{}, fmt.Errorf("tuitui: decrypt app secret: %w", err)
		}
		secret = strings.TrimSpace(plain)
	}
	c := Config{
		AppID:            strings.TrimSpace(ic.AppID),
		AppSecret:        secret,
		Host:             strings.TrimSpace(ic.Host),
		Port:             ic.Port,
		MaxMessageLength: ic.MaxMessageLength,
		ReactionEmoji:    strings.TrimSpace(ic.ReactionEmoji),
	}
	if c.AppID == "" {
		return Config{}, errors.New("tuitui: installation config missing app_id")
	}
	if c.AppSecret == "" {
		return Config{}, errors.New("tuitui: installation config missing app_secret")
	}
	if c.Host == "" {
		c.Host = DefaultHost
	}
	if c.Port == 0 {
		c.Port = DefaultPort
	} else if c.Port < 1 || c.Port > 65535 {
		return Config{}, fmt.Errorf("tuitui: invalid port %d", c.Port)
	}
	if c.MaxMessageLength <= 0 {
		c.MaxMessageLength = DefaultMaxMessageLength
	}
	if c.ReactionEmoji == "" {
		c.ReactionEmoji = DefaultReactionEmoji
	}
	return c, nil
}

// decryptToken base64-decodes the stored ciphertext (tolerating the MIME
// newline wrapping PostgreSQL's encode(...,'base64') emits) and runs it
// through the injected Decrypter. A nil Decrypter treats the decoded bytes as
// plaintext (test convenience). Ported from the DingTalk adapter's helper.
func decryptToken(enc string, decrypt Decrypter) (string, error) {
	if enc == "" {
		return "", nil
	}
	var b strings.Builder
	for _, r := range enc {
		switch r {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			b.WriteRune(r)
		}
	}
	ciphertext, err := base64.StdEncoding.DecodeString(b.String())
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	if decrypt == nil {
		return string(ciphertext), nil
	}
	plaintext, err := decrypt(ciphertext)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// wsURL is the inbound callback WebSocket endpoint. The credential rides in
// the "auth" query parameter as "{app_id}.{app_secret}".
func (c Config) wsURL() string {
	return fmt.Sprintf("wss://%s:%d/robot/callback/ws?auth=%s.%s", c.Host, c.Port, c.AppID, c.AppSecret)
}

// apiBase is the HTTP robot API root; per-call paths and the appid/secret
// query auth are appended by client.go.
func (c Config) apiBase() string {
	return fmt.Sprintf("https://%s:%d/robot", c.Host, c.Port)
}
