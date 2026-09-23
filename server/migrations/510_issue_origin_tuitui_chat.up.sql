-- Extend issue.origin_type for issues created by the Tuitui `/issue`
-- command. origin_id stores the chat_session id, matching the existing Lark,
-- Slack, DingTalk, WeCom, and Telegram channel origins.
--
-- The full list is respecified because ADD CONSTRAINT cannot append a value:
-- dropping any earlier entry would silently invalidate that channel's issues.
--
-- This only widens the allowed set, so every existing row already satisfies
-- it. Recreate the CHECK as NOT VALID so the ACCESS EXCLUSIVE lock is held
-- briefly; migration 511 performs the table scan under SHARE UPDATE
-- EXCLUSIVE without blocking normal reads and writes. The VALIDATE stays in
-- its own file (the 366/367 pattern) because the runner hands each file to a
-- single conn.Exec: one implicit transaction would carry migration 510's
-- strong lock straight through the validation scan.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'slack_chat', 'agent_create', 'dingtalk_chat', 'wecom_chat', 'telegram_chat', 'tuitui_chat'))
    NOT VALID;
