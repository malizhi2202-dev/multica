// Desktop-only helpers for the project_resource local_directory flow.
//
// These wrap the preload `desktopAPI` surface so view components can
// SSR-render on web (where `window.desktopAPI` is undefined) and degrade
// gracefully to no-op promises instead of crashing.

export type PickDirectoryResult = {
  ok: boolean;
  path?: string;
  basename?: string;
  reason?: "cancelled" | "no_window" | "error" | "unsupported";
  error?: string;
};

export type ValidateLocalDirectoryResult = {
  ok: boolean;
  reason?:
    | "not_absolute"
    | "not_found"
    | "not_a_directory"
    | "not_readable"
    | "not_writable"
    | "error"
    | "unsupported";
  error?: string;
  /**
   * Whether the directory sits inside a git working tree. Only meaningful when
   * ok=true; absent from an older desktop build, which is why callers must
   * treat `undefined` as "unknown" rather than "not a repo".
   */
  is_git_repo?: boolean;
};

interface DesktopLocalDirectoryAPI {
  pickDirectory?: (defaultPath?: string) => Promise<PickDirectoryResult>;
  validateLocalDirectory?: (
    path: string,
  ) => Promise<ValidateLocalDirectoryResult>;
}

function readDesktopAPI(): DesktopLocalDirectoryAPI | undefined {
  if (typeof window === "undefined") return undefined;
  const api = (window as unknown as { desktopAPI?: DesktopLocalDirectoryAPI })
    .desktopAPI;
  return api;
}

/** True when the renderer is running inside the Electron desktop shell, as
 *  evidenced by the preload-exposed pickDirectory bridge. Avoids hard-coding
 *  navigator/process checks — those vary across electron-vite + jsdom tests. */
export function isDesktopShell(): boolean {
  const api = readDesktopAPI();
  return typeof api?.pickDirectory === "function";
}

/**
 * Which way the current context can actually complete a local_directory pick.
 *
 * `desktop_picker` — the preload native folder picker (the original desktop
 * path). `server_browser` — the deployment server exposes its own filesystem
 * via GET /api/workspaces/{id}/local-dirs and resolves the daemon id there.
 * `manual_path` — type an absolute path; a locally registered daemon is the
 * one the resource gets pinned to, and the server's own save-time checks are
 * the validation. `read_only` — nothing in this environment can produce the
 * daemon_id the resource must be bound to, so offer no dead-end button.
 */
export type LocalDirectoryCapability =
  | "desktop_picker"
  | "server_browser"
  | "manual_path"
  | "read_only";

/**
 * Resolves the strongest available capability. Like `isDesktopShell()` this
 * is a capability probe, not a platform check: each tier is answered by a
 * live signal (preload bridge, server-declared boolean, daemon status), so a
 * desktop build never loses its native picker when the server gains the
 * browse endpoint, and a web build gains one the moment the server declares
 * it. Preference order is deliberate — native first (best UX, fully offline),
 * server browser second (works from any browser, but depends on the
 * deployment's filesystem), manual last (only viable with a local daemon).
 */
export function resolveLocalDirectoryCapability(input: {
  desktopPickerAvailable: boolean;
  serverBrowserSupported: boolean;
  localDaemonAvailable: boolean;
}): LocalDirectoryCapability {
  if (input.desktopPickerAvailable) return "desktop_picker";
  if (input.serverBrowserSupported) return "server_browser";
  if (input.localDaemonAvailable) return "manual_path";
  return "read_only";
}

export async function pickDirectory(
  defaultPath?: string,
): Promise<PickDirectoryResult> {
  const api = readDesktopAPI();
  if (!api?.pickDirectory) return { ok: false, reason: "unsupported" };
  return api.pickDirectory(defaultPath);
}

export async function validateLocalDirectory(
  path: string,
): Promise<ValidateLocalDirectoryResult> {
  const api = readDesktopAPI();
  if (!api?.validateLocalDirectory) return { ok: false, reason: "unsupported" };
  return api.validateLocalDirectory(path);
}
