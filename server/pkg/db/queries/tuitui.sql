-- Tuitui-specific chat-channel queries. Like dingtalk.sql the underlying
-- channel_* tables are shared and selected by channel_type, and these statements
-- deliberately stay out of the shared channel query surface. Nothing here adds a
-- table: Tuitui has no platform-side group discovery, so the Settings inventory is
-- projected from the generic session bindings.

-- name: ListTuituiGroupSessionsByWorkspace :many
-- Every group conversation this workspace's Tuitui bot currently has a live route
-- for, with the chat it maps to. Retired generations stay out (history, not a
-- route) and a revoked installation stays out, so disconnecting a bot empties the
-- inventory the same way it stops the connection.
SELECT b.id AS binding_id, b.channel_chat_id, b.installation_id, ci.agent_id,
       cs.title, cs.id AS chat_session_id, cs.updated_at
FROM channel_chat_session_binding b
JOIN channel_installation ci ON ci.id = b.installation_id
JOIN chat_session cs ON cs.id = b.chat_session_id
WHERE b.channel_type = 'tuitui'
  AND b.chat_type = 'group'
  AND b.retired_at IS NULL
  AND ci.workspace_id = sqlc.arg('workspace_id')
  AND ci.channel_type = 'tuitui'
  AND ci.status = 'active'
  AND (sqlc.arg('filter_by_agent')::boolean IS NOT TRUE OR ci.agent_id = sqlc.arg('agent_id')::uuid)
ORDER BY cs.updated_at DESC, b.channel_chat_id ASC;

-- name: ForgetTuituiGroupSession :one
-- Retire one live group route, scoped to the workspace + installation so a forged
-- conversation id cannot touch another bot's session. Returns the retired row;
-- zero rows means "nothing live matches" and the handler answers 404. The
-- chat_session and its messages are retained, exactly like the DingTalk presence
-- forget: the next addressed message from that group opens a fresh session.
UPDATE channel_chat_session_binding b
SET retired_at = now()
FROM channel_installation ci
WHERE b.installation_id = ci.id
  AND ci.id = sqlc.arg('installation_id')
  AND ci.workspace_id = sqlc.arg('workspace_id')
  AND ci.channel_type = 'tuitui'
  AND b.channel_type = 'tuitui'
  AND b.chat_type = 'group'
  AND b.channel_chat_id = sqlc.arg('channel_chat_id')::text
  AND b.retired_at IS NULL
RETURNING b.*;

-- name: ListTuituiUserBindingsForMember :many
-- Returns only the requesting Multica member's Tuitui identities. The
-- installation list is member-visible, so returning every member's account here
-- would disclose identities more broadly than necessary.
SELECT installation_id, channel_user_id
FROM channel_user_binding
WHERE workspace_id = sqlc.arg('workspace_id')
  AND multica_user_id = sqlc.arg('multica_user_id')
  AND channel_type = 'tuitui'
ORDER BY bound_at DESC, id ASC;

-- name: ListTuituiAllowlistRejections :many
-- The people and conversations this workspace's Tuitui bots refused because
-- none of the bot's allowlists named them, newest first and collapsed to one
-- row per refused subject. channel_inbound_audit is the shared non-content
-- drop audit and carries no workspace column, so the join to
-- channel_installation is what scopes a read to this workspace; the reason
-- label arrives as a parameter so the constant keeps one home in Go.
-- is_direct tells a person (a refused DM, keyed by user account) from a group
-- or teams channel (keyed by the channel-scoped conversation id) — exactly the
-- two lists the Settings panel writes them into. The optional installation
-- filter is how one bot's dialog shows only its own refusals.
SELECT a.channel_chat_id AS subject_id,
       a.installation_id,
       ci.agent_id,
       bool_or(a.event_type = 'single_chat') AS is_direct,
       count(*)::bigint AS seen_count,
       max(a.received_at)::timestamptz AS last_seen_at
FROM channel_inbound_audit a
JOIN channel_installation ci ON ci.id = a.installation_id
WHERE a.channel_type = 'tuitui'
  AND a.drop_reason = sqlc.arg('drop_reason')
  AND a.channel_chat_id IS NOT NULL
  AND ci.channel_type = 'tuitui'
  AND ci.status = 'active'
  AND ci.workspace_id = sqlc.arg('workspace_id')
  AND (sqlc.arg('filter_by_installation')::boolean IS NOT TRUE
       OR a.installation_id = sqlc.arg('installation_id')::uuid)
  -- Offer only what the operator has not already allowed: an audit row records
  -- the moment of refusal and never expires, so without this the id just added
  -- would still be on the list and the dialog would read as if the save had not
  -- taken. The rule mirrors the inbound gate on the same blob — a list that is
  -- absent or carries the wildcard covers every subject. A conversation can also
  -- be reached through its sender's personal entry, and the audit row keeps no
  -- sender, so that route is only recognised here when the personal list is
  -- unrestricted as a whole.
  AND NOT (CASE WHEN a.event_type = 'single_chat' THEN
             jsonb_exists(COALESCE(ci.config -> 'allow_from', '[]'::jsonb), a.channel_chat_id)
             OR jsonb_exists(COALESCE(ci.config -> 'allow_from', '[]'::jsonb), '*')
             OR COALESCE(jsonb_typeof(ci.config -> 'allow_from'), 'null') <> 'array'
           ELSE
             jsonb_exists(COALESCE(ci.config -> 'group_allow_from', '[]'::jsonb), a.channel_chat_id)
             OR jsonb_exists(COALESCE(ci.config -> 'group_allow_from', '[]'::jsonb), '*')
             OR COALESCE(jsonb_typeof(ci.config -> 'group_allow_from'), 'null') <> 'array'
             OR jsonb_exists(COALESCE(ci.config -> 'allow_from', '[]'::jsonb), '*')
             OR COALESCE(jsonb_typeof(ci.config -> 'allow_from'), 'null') <> 'array'
           END)
GROUP BY a.channel_chat_id, a.installation_id, ci.agent_id
ORDER BY max(a.received_at) DESC, a.channel_chat_id ASC
LIMIT sqlc.arg('row_limit')::int OFFSET sqlc.arg('row_offset')::int;

-- name: SetTuituiInstallationAllowlists :one
-- Replaces (or, when an argument is NULL, leaves alone) one side of an
-- installation's inbound allowlist, in place.
--
-- The `config || jsonb_build_object(...)` form is what keeps this safe: jsonb
-- merging carries every other key over untouched — including the sealed
-- app_secret_encrypted, host, port and app_id — so there is no Go-side
-- read-modify-write of the blob. That matters twice over: a Go round trip
-- through a struct would silently drop any key the struct does not declare
-- (which would make an existing bot's credentials undecryptable), and two
-- overlapping writes would each be computed from a stale read and lose the
-- other's key. Here both sides are one atomic UPDATE of one row, so concurrent
-- writes to different keys both land, and concurrent writes to the same key are
-- last-write-wins, which is what editing a form means.
--
-- NULL means "this request said nothing about this list", distinct from '[]'
-- ("the operator cleared it: nobody") and from '["*"]' ("everyone").
UPDATE channel_installation
SET config = config
      || CASE WHEN sqlc.narg('allow_from')::jsonb IS NOT NULL
              THEN jsonb_build_object('allow_from', sqlc.narg('allow_from')::jsonb)
              ELSE '{}'::jsonb END
      || CASE WHEN sqlc.narg('group_allow_from')::jsonb IS NOT NULL
              THEN jsonb_build_object('group_allow_from', sqlc.narg('group_allow_from')::jsonb)
              ELSE '{}'::jsonb END,
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND workspace_id = sqlc.arg('workspace_id')
  AND channel_type = 'tuitui'
RETURNING *;
