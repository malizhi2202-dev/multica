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
