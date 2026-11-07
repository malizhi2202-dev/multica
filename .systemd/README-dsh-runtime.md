# DeepSeek Harness (dsh) runtime

The daemon drives DSH through `dsh --profile multica --stdio`, so DSH needs the
Multica runtime profile installed before the runtime shows up. These are the
paths the `multica-daemon` unit points at:

| What | Path |
| --- | --- |
| dsh CLI (npm `@deepseek-ai/dsh@0.2.1-rc.2`) | `/home/malizhi/opt/dsh-multica/node_modules/.bin/dsh` |
| Isolated DSH home (profile `multica`) | `/home/malizhi/.dsh-multica` |
| Runtime bridge bundle | `/home/malizhi/opt/dsh-multica/multica-ai-dsh-runtime-0.2.0-private.1.tgz` |
| Bridge source (rebuild from here) | `/home/malizhi/ai/dsh-multica-runtime` |

The home is deliberately separate from `~/.dsh`: the interactive DSH is the dev
checkout on another version line, and `$DSH_HOME/profiles/node_modules` is
healed on every launch by whichever installation boots. Two installations in
one home would overwrite each other's module fallback.

## Verify

```bash
export DSH_HOME=/home/malizhi/.dsh-multica
/home/malizhi/opt/dsh-multica/node_modules/.bin/dsh --profile multica --probe
# {"v":1,"type":"probe","runtime":"dsh","plugin_version":"…","protocol_version":1}

curl -s http://127.0.0.1:19514/health | jq .agents   # expect "dsh"
```

## Upgrade

Rebuild the bridge, pack it, reinstall the profile, then restart the daemon:

```bash
cd /home/malizhi/ai/dsh-multica-runtime && git pull && pnpm install && pnpm build && pnpm pack
cp multica-ai-dsh-runtime-*.tgz /home/malizhi/opt/dsh-multica/
DSH_HOME=/home/malizhi/.dsh-multica /home/malizhi/opt/dsh-multica/node_modules/.bin/dsh \
  plugin --profile multica add /home/malizhi/opt/dsh-multica/multica-ai-dsh-runtime-<version>.tgz
./multica.sh install && ./multica.sh restart
```

Bump `MULTICA_DSH_PROFILE_BUNDLE` in `.systemd/multica-daemon.service` to the
new tarball in the same change — it is what the daemon reinstalls when the
profile goes missing.

The bridge declares the DSH versions it supports (`engines.dsh`); installing it
into a dsh outside that range makes DSH refuse to load the profile, and
`--probe` stops answering.
