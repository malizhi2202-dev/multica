-- Deployment-level secret storage. One row per logical key; the integration DEK
-- is currently the only one. No FKs, no cascades (MUL-3515 §4): the row outlives
-- every installation whose credential it seals, and deleting it is a destructive
-- administrative act, never a side effect of a delete elsewhere.

-- name: InsertInstanceSecretIfAbsent :exec
-- First-boot race guard: concurrent processes each generate their own candidate
-- key, and only the winner's row survives. The caller re-reads afterwards with
-- GetInstanceSecret so every racer converges on ONE key — two nodes minting two
-- DEKs would leave half the stored ciphertext unopenable.
INSERT INTO instance_secret (key_name, secret)
VALUES (sqlc.arg('key_name'), sqlc.arg('secret'))
ON CONFLICT (key_name) DO NOTHING;

-- name: GetInstanceSecret :one
SELECT secret FROM instance_secret WHERE key_name = $1;
