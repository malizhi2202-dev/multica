// Package tuitui is the Tuitui (推推) IM integration for the channel-agnostic
// engine. It uses the bring-your-own-app (BYO) model like DingTalk: an agent
// owner creates a Tuitui robot application, and Multica stores its app_id /
// app_secret per channel_installation. Each installation runs its own
// inbound WebSocket long connection (wss://{host}:{port}/robot/callback/ws?
// auth={app_id}.{app_secret}) supervised by the shared engine, and replies
// over the HTTP robot API (https://{host}:{port}/robot/message/custom/send,
// query-string authenticated with appid/secret — NOT headers).
//
// Platform facts this adapter encodes, verified against a Python reference
// client for the same Tuitui robot API:
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
	"unicode"
	"unicode/utf8"

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

	// AllowFrom / GroupAllowFrom gate which senders and conversations may
	// reach the core at all (see allow.go for the decision rule and why an
	// absent key imposes no restriction).
	AllowFrom      Allowlist
	GroupAllowFrom Allowlist
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
	// AllowFrom gates DMs (Tuitui user accounts); GroupAllowFrom gates group
	// and teams conversations by their conversation id. Same key names and the
	// same list-of-strings value shape as the reference bridge client's
	// config.bridge section, but stored normalized (comma strings are split on
	// write by the handler, never here).
	//
	// The pointer plus omitempty is what keeps "not configured" and
	// "configured as nobody" distinguishable in the stored JSON: absent key (or
	// null) writes nothing and imposes nothing, while an explicit [] is written
	// as an empty array and denies everyone. A bare []string with omitempty
	// would collapse both into "no key".
	AllowFrom      *[]string `json:"allow_from,omitempty"`
	GroupAllowFrom *[]string `json:"group_allow_from,omitempty"`
}

// AllowWildcard is the entry that opens one list to everyone, mirroring the
// reference bridge client's config docs ("* = allow all").
const AllowWildcard = "*"

// MaxAllowlistEntries / MaxAllowlistEntryLen bound one stored list. They are
// write-path validation only: a Settings submit with more entries is a 400 the
// dialog can show rather than an unbounded JSON blob in channel_installation.
const (
	MaxAllowlistEntries  = 500
	MaxAllowlistEntryLen = 128
)

// AllowlistMode is the verdict of one allowlist field as STORED, which is a
// three-way distinction the adapter must never collapse:
//
//   - AllowlistUnset: config has no key at all (every installation created
//     before this feature, plus an explicit JSON null). Imposes no
//     restriction — see Allowlist for why this is the only safe default.
//   - AllowlistAll: the stored list contains "*", including a mixed "*" plus
//     concrete ids, which is what the dialog prefills.
//   - AllowlistSome: concrete ids only.
//   - AllowlistNone: the key is present and the list is empty: fail closed.
type AllowlistMode int

const (
	AllowlistUnset AllowlistMode = iota
	AllowlistAll
	AllowlistSome
	AllowlistNone
)

// Allowlist is one normalized inbound allowlist field (allow_from or
// group_allow_from). Entries carries only the concrete ids; the wildcard is
// recorded as Mode, never as an entry, so nothing downstream has to read that
// symbol again.
type Allowlist struct {
	Mode    AllowlistMode
	Entries []string
}

// Allows reports whether id passes this list. The comparison is exact and
// case-sensitive, like the reference bridge client's membership test.
func (a Allowlist) Allows(id string) bool {
	switch a.Mode {
	case AllowlistAll:
		return true
	case AllowlistSome:
		for _, entry := range a.Entries {
			if entry == id {
				return true
			}
		}
		return false
	case AllowlistNone:
		return false
	default: // AllowlistUnset
		return true
	}
}

// Unrestricted reports whether the list lets every subject through: either the
// operator asked for it ("*") or never configured one.
func (a Allowlist) Unrestricted() bool { return a.Mode == AllowlistAll || a.Mode == AllowlistUnset }

// Configured reports whether the stored config carried the key at all, which is
// what the API echo uses to tell "this bot has a list" from "nobody ever set
// one up".
func (a Allowlist) Configured() bool { return a.Mode != AllowlistUnset }

// Stored returns the JSON value for one list, or nil when it is unconfigured so
// installConfig's omitempty leaves the key out and the row keeps the
// not-configured shape. An AllowlistAll is written back as the bare wildcard.
func (a Allowlist) Stored() *[]string {
	switch a.Mode {
	case AllowlistUnset:
		return nil
	case AllowlistAll:
		entries := []string{AllowWildcard}
		return &entries
	case AllowlistSome:
		entries := append([]string(nil), a.Entries...)
		return &entries
	default: // AllowlistNone
		entries := []string{}
		return &entries
	}
}

// allowlistFromStored resolves one decoded config key into its Allowlist. The
// *[]string shape is what keeps the three cases distinguishable: nil pointer =
// key absent (or null), pointer to an empty slice = explicit empty list,
// pointer to entries = a list. A plain []string could not tell the first two
// apart without leaning on Go's nil-versus-empty accident.
func allowlistFromStored(stored *[]string) Allowlist {
	if stored == nil {
		return Allowlist{Mode: AllowlistUnset}
	}
	entries := make([]string, 0, len(*stored))
	for _, entry := range *stored {
		entry = strings.TrimSpace(entry)
		if entry != "" {
			entries = append(entries, entry)
		}
	}
	return allowlistFromEntries(entries)
}

// allowlistFromEntries classifies an already-trimmed, non-empty entry list.
// Wildcard wins over everything stored beside it: the reference bridge client
// tests membership of "*" first, so concrete entries behind it are inert and
// are dropped rather than echoed as if they still restricted anything.
func allowlistFromEntries(entries []string) Allowlist {
	for _, entry := range entries {
		if entry == AllowWildcard {
			return Allowlist{Mode: AllowlistAll}
		}
	}
	entries = dedupeKeepOrder(entries)
	if len(entries) == 0 {
		return Allowlist{Mode: AllowlistNone}
	}
	return Allowlist{Mode: AllowlistSome, Entries: entries}
}

// dedupeKeepOrder collapses repeated ids without re-sorting, so what the user
// typed stays in the same order on screen.
func dedupeKeepOrder(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// ParseAllowlistCSV normalizes one comma-separated dialog field into the
// entries to store: trimmed, empty items dropped, duplicates collapsed, and a
// lone "*" when the wildcard appears anywhere. It is the validation the write
// path shares with AllowlistFromCSV, exported so a handler can report the
// precise violation as a 400 rather than a catch-all failure.
func ParseAllowlistCSV(field string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == AllowWildcard {
			return []string{AllowWildcard}, nil
		}
		if n := utf8.RuneCountInString(part); n > MaxAllowlistEntryLen {
			return nil, fmt.Errorf("an allowlist entry is too long (%d characters; the limit is %d)", n, MaxAllowlistEntryLen)
		}
		if strings.IndexFunc(part, unicode.IsControl) >= 0 {
			return nil, errors.New("an allowlist entry contains a control character")
		}
		out = append(out, part)
	}
	out = dedupeKeepOrder(out)
	if len(out) > MaxAllowlistEntries {
		return nil, fmt.Errorf("too many allowlist entries (%d; the limit is %d)", len(out), MaxAllowlistEntries)
	}
	return out, nil
}

// AllowlistFromCSV turns one Settings dialog field into the stored tri-state.
// An empty or wildcard-free field that normalizes to nothing is
// AllowlistNone — "configured, and nobody is on it" — which is why the write
// path must distinguish an absent field from an empty one before calling this.
func AllowlistFromCSV(field string) (Allowlist, error) {
	entries, err := ParseAllowlistCSV(field)
	if err != nil {
		return Allowlist{}, err
	}
	return allowlistFromEntries(entries), nil
}

// AllowlistsFromStoredConfig decodes just the two allowlist keys out of a
// stored channel_installation.config blob, for the management surface that
// echoes the effective lists without decrypting any credential. A torn or
// pre-feature blob reads back as "not configured", which is precisely what the
// inbound gate applies to the same blob — echo and enforcement cannot
// disagree, because both go through allowlistFromStored.
func AllowlistsFromStoredConfig(raw []byte) (direct, group Allowlist) {
	notConfigured := Allowlist{Mode: AllowlistUnset}
	var ic installConfig
	if len(raw) == 0 || json.Unmarshal(raw, &ic) != nil {
		return notConfigured, notConfigured
	}
	return allowlistFromStored(ic.AllowFrom), allowlistFromStored(ic.GroupAllowFrom)
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
		AllowFrom:        allowlistFromStored(ic.AllowFrom),
		GroupAllowFrom:   allowlistFromStored(ic.GroupAllowFrom),
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
