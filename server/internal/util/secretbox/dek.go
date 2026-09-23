package secretbox

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// IntegrationDEKName is the instance_secret row holding the shared data-encryption
// key every chat-channel integration seals its per-installation credentials
// with. Versioned in the name on purpose: a future rotation writes
// multica.integration-dek.v2 beside it and the rows that were sealed by v1 stay
// readable while a migration re-seals them.
const IntegrationDEKName = "multica.integration-dek.v1"

// Material is a resolved master key in both forms callers need. Box seals and
// opens per-installation credentials; Key is the raw bytes, which the Plugin
// service also needs for HMAC derivation (a hook signature must be reproducible
// on demand, and an AEAD cannot produce one) — so a caller that only gets a Box
// cannot be adapted to that use later.
type Material struct {
	Key []byte
	Box *Box
}

// ErrNoKeySource is returned when neither the legacy deployment env var nor a
// usable stored DEK is available and the caller passed no queries. It is the
// only way integration startup can end up with no encryption key at all, and
// callers handle it exactly as they handled a missing env var before: that
// integration stays disabled, loudly, in the log.
var ErrNoKeySource = errors.New("secretbox: no master key available (environment unset and no stored DEK)")

// ResolveIntegrationKey returns the master key one integration should use, plus
// the Box built from it.
//
// Order of precedence:
//
//  1. legacyEnvVar, when that variable is set. The whole env path stays intact —
//     base64 decode, 32-byte check, and the same error text for a malformed
//     value — so a deployment that already provisioned its own key keeps a
//     stronger guarantee and survives the day the automatic key appears.
//  2. otherwise the deployment's data-encryption key from instance_secret: read
//     it, or on first boot generate 32 random bytes, insert them
//     ON CONFLICT DO NOTHING, and read again. The re-read is what makes the
//     first boot of several replicas converge on ONE key; two nodes that each
//     kept their own generated key would leave half the stored ciphertext
//     permanently unopenable.
//
// A resolved key is never cached by name and never re-read per request: like the
// env behaviour it replaces, the value is taken once at boot and held by the
// caller's Box for the process lifetime.
func ResolveIntegrationKey(ctx context.Context, legacyEnvVar string, q *db.Queries) (Material, error) {
	if raw := os.Getenv(legacyEnvVar); raw != "" {
		key, err := LoadKey(legacyEnvVar)
		if err != nil {
			return Material{}, err
		}
		return newMaterial(key)
	}
	if q == nil {
		return Material{}, fmt.Errorf("%w for %s", ErrNoKeySource, legacyEnvVar)
	}
	key, err := q.GetInstanceSecret(ctx, IntegrationDEKName)
	if errors.Is(err, pgx.ErrNoRows) {
		key, err = generateAndStoreDEK(ctx, q)
		if err != nil {
			return Material{}, err
		}
	} else if err != nil {
		return Material{}, fmt.Errorf("secretbox: read %s: %w", IntegrationDEKName, err)
	}
	if len(key) != KeySize {
		return Material{}, fmt.Errorf("secretbox: stored DEK %s is %d bytes, expected %d", IntegrationDEKName, len(key), KeySize)
	}
	return newMaterial(key)
}

// generateAndStoreDEK mints a candidate key and converges on the winner. The
// insert deliberately does not overwrite, so exactly one process's candidate
// survives; everyone then reads that one back. A read that still comes up empty
// (the winner crashed between generate and commit) is an error rather than a
// silently different key — the next boot retries.
func generateAndStoreDEK(ctx context.Context, q *db.Queries) ([]byte, error) {
	candidate := make([]byte, KeySize)
	if _, err := rand.Read(candidate); err != nil {
		return nil, fmt.Errorf("secretbox: generate DEK: %w", err)
	}
	if err := q.InsertInstanceSecretIfAbsent(ctx, db.InsertInstanceSecretIfAbsentParams{
		KeyName: IntegrationDEKName,
		Secret:  candidate,
	}); err != nil {
		return nil, fmt.Errorf("secretbox: store DEK: %w", err)
	}
	key, err := q.GetInstanceSecret(ctx, IntegrationDEKName)
	if err != nil {
		return nil, fmt.Errorf("secretbox: re-read DEK: %w", err)
	}
	return key, nil
}

func newMaterial(key []byte) (Material, error) {
	box, err := New(key)
	if err != nil {
		return Material{}, err
	}
	return Material{Key: key, Box: box}, nil
}
