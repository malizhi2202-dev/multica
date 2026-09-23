# Master key: legacy env var, else the stored DEK

`ResolveIntegrationKey(ctx, legacyEnvVar, queries)` returns `Material{Key, Box}`:
env var first, else `instance_secret.key_name = multica.integration-dek.v1`, minted
once via `ON CONFLICT DO NOTHING` + re-read so replicas converging on a first boot
settle on ONE key. `ErrNoKeySource` only with no env and no database; `LoadKey` and
every env variable name are unchanged.

Sites (all `cmd/server/router.go`, grep `ResolveIntegrationKey`): Lark, Slack,
DingTalk, Tuitui, WeCom, Telegram, VCS, Plugins. No key → `slog.Info`, block
unwired, handlers 403 on their nil-service check; a pool-less router passes none.

Couplings: read once at boot, so `configured` / `*_supported` now mean "usable",
not "env set" (client copy to review). VCS keeps `MULTICA_VCS_INTEGRATION_ENABLED`;
Plugins also needs the raw `Material.Key` (`DeploymentKey`, `NewPluginSurfaceTokenBox`).
