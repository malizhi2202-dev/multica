-- The deployment's data-encryption key (DEK). One row holds the 32-byte AES-256
-- key every chat-channel integration seals its per-installation credentials
-- with, so pasting an app id + secret in the Settings UI works on a deployment
-- where the operator set no key in the environment
-- (server/internal/util/secretbox/MIGRATION.md).
--
-- Deliberately minimal: no foreign keys (MUL-3515 §4 keeps this table
-- independent of every row it protects), and no secondary index because the
-- primary key is the only access path and one row is read once per process
-- start. The table carries no workspace_id, so it is deployment-global and
-- workspace teardown keeps it (see workspaceDeletionManifest in the handler
-- tests). IF NOT EXISTS keeps a re-run safe if the object already exists; the
-- matching down migration keeps the row, since dropping the key would make
-- every ciphertext already stored by an integration permanently unopenable.
CREATE TABLE IF NOT EXISTS instance_secret (
    key_name TEXT NOT NULL,
    secret BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (key_name)
);
