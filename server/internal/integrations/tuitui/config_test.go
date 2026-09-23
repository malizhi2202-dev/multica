package tuitui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

func TestFromConfigRequiresCredentials(t *testing.T) {
	if _, err := FromConfig(channel.Config{}); err == nil {
		t.Fatal("empty Raw must error")
	}
	if _, err := FromConfig(channel.Config{Raw: json.RawMessage(`{"app_secret":"s"}`)}); err == nil {
		t.Fatal("missing app_id must error")
	}
	if _, err := FromConfig(channel.Config{Raw: json.RawMessage(`{"app_id":"a"}`)}); err == nil {
		t.Fatal("missing app_secret must error")
	}
	if _, err := FromConfig(channel.Config{Raw: json.RawMessage(`{not json`)}); err == nil {
		t.Fatal("malformed JSON must error")
	}
	if _, err := FromConfig(channel.Config{Raw: json.RawMessage(`{"app_id":"a","app_secret":"s","port":70000}`)}); err == nil {
		t.Fatal("out-of-range port must error")
	}
}

func TestFromConfigEncryptedSecret(t *testing.T) {
	// app_secret_encrypted holds base64(secretbox ciphertext); the injected
	// Decrypter turns it into the connection secret. The ciphertext here is
	// "s3cr3t" base64'd, decrypted by a stub Decrypter (no secretbox import
	// needed in the adapter).
	cipher := "czNjcjN0" // base64("s3cr3t")
	c, err := FromConfigDecrypted(
		channel.Config{Raw: json.RawMessage(`{"app_id":"a","app_secret_encrypted":"` + cipher + `"}`)},
		func(b []byte) ([]byte, error) { return b, nil },
	)
	if err != nil {
		t.Fatalf("decrypt path: %v", err)
	}
	if c.AppSecret != "s3cr3t" {
		t.Errorf("AppSecret = %q, want s3cr3t", c.AppSecret)
	}

	// A failing Decrypter surfaces as a config error, never a silent empty
	// secret.
	if _, err := FromConfigDecrypted(
		channel.Config{Raw: json.RawMessage(`{"app_id":"a","app_secret_encrypted":"` + cipher + `"}`)},
		func([]byte) ([]byte, error) { return nil, errors.New("wrong key") },
	); err == nil {
		t.Fatal("decrypt failure must error")
	}

	// Plaintext app_secret wins when both fields exist.
	c, err = FromConfigDecrypted(
		channel.Config{Raw: json.RawMessage(`{"app_id":"a","app_secret":"plain","app_secret_encrypted":"` + cipher + `"}`)},
		func(b []byte) ([]byte, error) { return b, nil },
	)
	if err != nil || c.AppSecret != "plain" {
		t.Errorf("plaintext precedence: %q %v", c.AppSecret, err)
	}
}

func TestFromConfigDefaults(t *testing.T) {
	c, err := FromConfig(channel.Config{Raw: json.RawMessage(`{"app_id":"a","app_secret":"s"}`)})
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if c.Host != DefaultHost || c.Port != DefaultPort {
		t.Errorf("host/port = %s:%d", c.Host, c.Port)
	}
	if c.MaxMessageLength != DefaultMaxMessageLength || c.ReactionEmoji != DefaultReactionEmoji {
		t.Errorf("maxlen/emoji = %d %q", c.MaxMessageLength, c.ReactionEmoji)
	}
	if !strings.HasPrefix(c.wsURL(), "wss://im.live.360.cn:8282/robot/callback/ws?auth=a.s") {
		t.Errorf("wsURL = %s", c.wsURL())
	}
	if c.apiBase() != "https://im.live.360.cn:8282/robot" {
		t.Errorf("apiBase = %s", c.apiBase())
	}
}

func TestFactoryMissingCredentials(t *testing.T) {
	factory := NewFactory(ChannelDeps{})
	if _, err := factory(channel.Config{Type: TypeTuitui, Raw: json.RawMessage(`{"app_id":"a"}`)}); err == nil {
		t.Fatal("factory must reject missing app_secret")
	}
	ch, err := factory(channel.Config{Type: TypeTuitui, Raw: json.RawMessage(`{"app_id":"a","app_secret":"s"}`)})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if ch.Type() != channel.Type("tuitui") {
		t.Errorf("Type = %q", ch.Type())
	}
	if err := ch.Connect(context.Background()); err == nil {
		t.Error("Connect without a handler must error")
	}
	if err := ch.Disconnect(context.Background()); err != nil {
		t.Errorf("Disconnect must be nil-safe: %v", err)
	}
}

func TestRegisterTuituiInRegistry(t *testing.T) {
	reg := channel.NewRegistry()
	RegisterTuitui(reg, ChannelDeps{})
	got, err := reg.Build(channel.Config{Type: TypeTuitui, Raw: json.RawMessage(`{"app_id":"a","app_secret":"s"}`)})
	if err != nil {
		t.Fatalf("registry build: %v", err)
	}
	if got.Type() != TypeTuitui {
		t.Errorf("type = %q", got.Type())
	}
}

func TestSeenEventDedup(t *testing.T) {
	ch := New(baseConfig(), nil, ConfigDeps{})
	if ch.seenEvent("e1") {
		t.Error("first sighting must not be marked seen")
	}
	if !ch.seenEvent("e1") {
		t.Error("second sighting must be seen")
	}
	for i := 0; i < maxSeenEvents+5; i++ {
		ch.seenEvent(string(rune('a'+i%26)) + string(rune('0'+i/26)) + "-x")
	}
	if len(ch.seenIDs) > maxSeenEvents {
		t.Errorf("seen set unbounded: %d", len(ch.seenIDs))
	}
}

func TestNormalizeKeepsRawRoundTrip(t *testing.T) {
	f := frameJSON(t, "single_chat", `{"msgid":"m","msg_type":"text","text":"x","at_me":true}`)
	n, ok := normalizeEvent(f, "app-stamp-1")
	if !ok {
		t.Fatal("want ok")
	}
	if len(n.msg.Raw) == 0 {
		t.Fatal("Raw payload must carry the untouched event body")
	}
	raw, err := decodeTuituiRaw(n.msg)
	if err != nil {
		t.Fatalf("decode stamped Raw: %v", err)
	}
	if raw.AppID != "app-stamp-1" {
		t.Errorf("stamped app_id = %q, want app-stamp-1 — the installation resolver routes on it", raw.AppID)
	}
	if raw.Body.Event != "single_chat" || raw.Body.UserAccount != "alice" {
		t.Errorf("Raw body round trip lost fields: %+v", raw.Body)
	}
	var back map[string]any
	if err := json.Unmarshal(n.msg.Raw, &back); err != nil {
		t.Fatalf("Raw must be JSON: %v", err)
	}
	body, _ := back["body"].(map[string]any)
	if _, ok := body["data"].(map[string]any)["at_me"]; !ok {
		t.Errorf("Raw body.data must keep the untouched platform payload: %s", n.msg.Raw)
	}
}
