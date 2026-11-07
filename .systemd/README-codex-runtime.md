# Codex runtime (OpenAI Codex CLI)

The daemon drives Codex through `codex app-server --listen stdio://`, so it needs
a Codex CLI on a path the daemon can resolve. The CLI installed by the
ChatGPT/Codex VS Code extension is reached through a stable symlink, and the
daemon unit pins that path with `MULTICA_CODEX_PATH`:

| What | Path |
| --- | --- |
| Codex CLI (VS Code extension build) | `/home/malizhi/.vscode-server/extensions/openai.chatgpt-26.1021.22027/bin/linux-x86_64/codex` |
| Stable symlink the daemon uses | `/home/malizhi/.local/bin/codex` |
| Daemon env override | `MULTICA_CODEX_PATH=/home/malizhi/.local/bin/codex` |
| Codex home (auth, config, sessions) | `/home/malizhi/.codex` |

The runtime identity is `codex` (protocol family `codex`, launch skeleton
`codex app-server`), registered automatically by the daemon's probe — no
workspace runtime profile is created by hand.

## Verify

```bash
~/.local/bin/codex --version
# {"v":1,...} initialize reply means app-server speaks the protocol the daemon drives
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"probe","version":"0.0.1"}}}' \
  | ~/.local/bin/codex app-server --listen stdio://

curl -s http://127.0.0.1:19514/health | jq .agents        # expect "codex" in the list
curl -s http://127.0.0.1:19514/health | jq '.workspaces'  # its runtime id appears per workspace
```

The UI shows it as `Codex (<device>)` under workspace runtimes, online.

## Upgrade

The extension build path is versioned, so an extension upgrade leaves the
symlink dangling:

```bash
ls -d ~/.vscode-server/extensions/openai.chatgpt-*/bin/linux-x86_64/codex | tail -1
ln -sf <that path> ~/.local/bin/codex
~/.local/bin/codex --version
systemctl --user restart multica-daemon
```

`MULTICA_CODEX_MODEL` can pin a model; without it Codex uses
`~/.codex/config.toml` (currently `gpt-6-sol`).

## Notes

- Bubblewrap is not installed on this host; Codex falls back to its bundled
  sandbox and logs a `configWarning` on startup. That warning is expected here
  and does not block tasks.
- The extension build reports `codex-cli 0.164.0-alpha.11.1`; multica's codex
  backend is version-tolerant across the 0.14x/0.16x app-server protocol, and
  the initialize/thread handshake was verified against this build.
