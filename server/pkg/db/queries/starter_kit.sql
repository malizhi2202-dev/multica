-- Starter kit: the bundled skills, agents, and squads imported into a
-- workspace once. The imports themselves reuse the ordinary CreateSkill /
-- CreateAgent / CreateSquad / AddAgentSkill / AddSquadMember statements; only
-- the marker bookkeeping, the per-workspace identity lookups this package needs
-- (agent by name — skills already have GetSkillByWorkspaceAndName), and the
-- startup sweep's listings live here.

-- name: ClaimStarterKitSeed :execrows
-- The one-shot gate. A workspace's first importer claims the row and does the
-- work in the same transaction; every later call sees zero rows and returns.
-- Rolling back the importer's transaction releases the claim with it, so an
-- interrupted run is retried rather than lost.
INSERT INTO starter_kit_seed (workspace_id, kit_key, kit_version)
VALUES ($1, $2, $3)
ON CONFLICT (workspace_id, kit_key) DO NOTHING;

-- name: GetStarterKitSeed :one
SELECT workspace_id, kit_key, kit_version, seeded_at
FROM starter_kit_seed
WHERE workspace_id = $1 AND kit_key = $2;

-- name: ListAllWorkspaceIDs :many
-- Startup sweep only: every workspace, oldest first, on the narrowest
-- projection possible, in the shape ListDaemonWorkspaces keeps to.
SELECT id FROM workspace
ORDER BY created_at ASC, id ASC;

-- name: GetWorkspaceOwnerUserID :one
-- Imported rows need an author. The owner is the one member guaranteed to exist
-- in every workspace (CreateWorkspace inserts it in the same transaction); the
-- oldest wins if an operator has somehow minted a second.
SELECT user_id FROM member
WHERE workspace_id = $1 AND role = 'owner'
ORDER BY created_at ASC, id ASC
LIMIT 1;

-- name: GetAgentIDByWorkspaceAndName :one
-- Kit identity is the workspace-unique name, mirroring
-- GetSkillByWorkspaceAndName: the bundle never keys off an id it cannot know
-- ahead of import, and an existing row with the kit's name is reused rather
-- than overwritten, whatever its origin.
SELECT id FROM agent
WHERE workspace_id = $1 AND name = $2;

-- name: GetSquadIDByWorkspaceAndName :one
-- Unlike agent and skill, squad has no UNIQUE(workspace_id, name) index, so a
-- name clash is possible and this lookup is what stops the import from minting
-- a second 研发交付小队 beside the one the workspace already has. Oldest wins.
SELECT id FROM squad
WHERE workspace_id = $1 AND name = $2
ORDER BY created_at ASC, id ASC
LIMIT 1;
