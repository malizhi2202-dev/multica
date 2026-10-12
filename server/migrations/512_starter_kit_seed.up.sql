-- Records that a workspace has received a bundled kit (its starter skills,
-- agents, and squads) so the import runs exactly once per workspace instead of
-- on every process start.
--
-- The marker is the whole point: the kit's rows are workspace-owned copies that
-- admins rename, extend, and delete afterwards. Re-running the import would
-- resurrect deleted rows and mint second copies beside renamed ones, so the
-- import claims this row and its inserts share one transaction — a failed or
-- cancelled run leaves no marker and is therefore retried on the next start,
-- while a completed run never runs again.
--
-- `kit_key` rather than a bare boolean so a second, unrelated bundle can
-- coexist, and `kit_version` records which revision of the bundle landed for
-- support, not because a version change re-runs anything.
--
-- No foreign keys (MUL-3515 §4): the table outlives every workspace row it
-- describes, and workspace teardown deletes its own kit rows independently.
-- The composite primary key is the only access path, so no secondary index.
CREATE TABLE IF NOT EXISTS starter_kit_seed (
    workspace_id UUID NOT NULL,
    kit_key TEXT NOT NULL,
    kit_version TEXT NOT NULL,
    seeded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, kit_key)
);
