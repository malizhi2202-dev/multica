"use client";

import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  ArrowUp,
  ChevronRight,
  Folder,
  FolderOpen,
  Home,
  TriangleAlert,
} from "lucide-react";
import { ApiError, clientErrorMessage } from "@multica/core/api";
import { useConfigStore } from "@multica/core/config";
import { localDirBrowseOptions } from "@multica/core/projects";
import type {
  LocalDirEntry,
  LocalDirectoryExecutionMode,
} from "@multica/core/types";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Spinner } from "@multica/ui/components/ui/spinner";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@multica/ui/components/ui/tooltip";
import { useT } from "../../i18n/use-t";
import {
  LocalDirectoryModeOptions,
  type WorktreeUnavailableReason,
} from "./local-directory-mode-dialog";

/**
 * What a confirmed browse selection means to the caller: create a
 * local_directory resource pinned to the daemon the SERVER resolved for
 * `localPath` — deliberately not a browser-local daemon id, which is the
 * thing the web flow never had.
 */
export interface LocalDirectoryBrowserSelection {
  localPath: string;
  daemonId: string;
  label: string;
  mode: LocalDirectoryExecutionMode;
}

interface LocalDirectoryBrowserDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  wsId: string;
  /** Parent's create-in-progress flag; the dialog stays open until it lands. */
  saving?: boolean;
  /** Rejection from the parent's create mutation, shown beside the confirm. */
  errorMessage?: string;
  /** Differs per entry point (attach to a project vs. seed a new one). */
  confirmLabel: string;
  onConfirm: (selection: LocalDirectoryBrowserSelection) => void;
}

/** Last path segment, for the resource label. POSIX-only: this is the
 *  deployment machine's filesystem, and Windows paths are not what the
 *  browse endpoint ever returns. Exported for the manual-path tier, which
 *  labels its rows the same way. */
export function pathBasename(path: string): string {
  const trimmed = path.replace(/\/+$/, "");
  const segments = trimmed.split("/");
  return segments[segments.length - 1] || trimmed || "/";
}

/**
 * Web/desktop-browser replacement for the native folder picker: lists one
 * level of the DEPLOYMENT machine's filesystem, resolved by the server, and
 * pins the picked directory to the daemon the server says serves it.
 *
 * Opening states are all recoverable by navigating or re-typing the path, so
 * none of them blanks the dialog: a 403 explains the permission, any other
 * 4xx surfaces the server's own reason (that is the path validation this
 * client cannot perform), and a non-resolved daemon status keeps browsing
 * usable while disabling the save — the same "one server answer must gate
 * the write" rule `resolved` exists for.
 */
export function LocalDirectoryBrowserDialog({
  open,
  onOpenChange,
  wsId,
  saving = false,
  errorMessage,
  confirmLabel,
  onConfirm,
}: LocalDirectoryBrowserDialogProps) {
  const { t } = useT("projects");
  // null = "let the server choose" (its daemon user's home). Every
  // navigation — entry click, up, home, typed path — is just another value
  // here, and the query key makes it a fresh request.
  const [path, setPath] = useState<string | null>(null);
  const [pathInput, setPathInput] = useState("");
  const [mode, setMode] = useState<LocalDirectoryExecutionMode>("in_place");

  // The listing is transient UI state like the mode dialog's draft: reopening
  // the dialog starts at the server default, not at last time's folder.
  useEffect(() => {
    if (open) {
      setPath(null);
      setPathInput("");
      setMode("in_place");
    }
  }, [open]);

  const { data, error, isPending } = useQuery({
    ...localDirBrowseOptions(wsId, path ? { path } : {}),
    enabled: open,
  });

  const forbidden = error instanceof ApiError && error.status === 403;
  // Any other 4xx carries the server's reason — the endpoint validated the
  // path (not absolute / missing / unreadable), and that sentence is the one
  // worth showing because no client-side check could have produced it. A 5xx
  // or transport failure has no user-actionable message, so it degrades to
  // the generic one (MUL-6472: server internals never reach the UI).
  const browseReason =
    error && !forbidden
      ? (clientErrorMessage(error) ?? t(($) => $.resources.local_browser_error))
      : undefined;

  // Only `resolved` may save. The API boundary already downgrades unknown
  // status values to `ambiguous`; the empty-daemon_id re-check covers a
  // server that breaks the other half of the contract, and an empty `path`
  // covers the schema fallback for a wholly unreadable response.
  const daemonResolved =
    data !== undefined &&
    data.daemon_status === "resolved" &&
    data.daemon_id.length > 0 &&
    data.path.length > 0;

  // Git-ness of a server-side directory cannot be checked from here, so it
  // stays unknown — which the mode dialog treats permissively. The daemon
  // re-checks authoritatively on save, and the rejection returns through
  // `errorMessage`. `server_outdated` is still ours to know: a server too
  // old to gate execution_mode would silently run worktree requests in
  // place, and a browse-capable server is separately declared by
  // local_worktree_supported (the two booleans are independent contracts).
  const serverValidatesWorktree = useConfigStore(
    (state) => state.localWorktreeSupported,
  );
  const unavailableReason: WorktreeUnavailableReason | undefined =
    serverValidatesWorktree ? undefined : "server_outdated";

  const goTo = (next: string) => setPath(next);
  const goToInput = () => {
    const trimmed = pathInput.trim();
    if (trimmed) goTo(trimmed);
  };
  const goUp = () => {
    if (data?.parent) goTo(data.parent);
  };
  const goHome = () => {
    if (data?.home) goTo(data.home);
  };

  const confirm = () => {
    if (!data || !daemonResolved) return;
    onConfirm({
      localPath: data.path,
      daemonId: data.daemon_id,
      label: pathBasename(data.path),
      mode,
    });
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{t(($) => $.resources.local_browser_title)}</DialogTitle>
          <DialogDescription>
            {t(($) => $.resources.local_browser_description)}
          </DialogDescription>
        </DialogHeader>

        <div className="flex items-center gap-2 text-caption">
          <span className="shrink-0 font-medium text-muted-foreground">
            {t(($) => $.resources.local_browser_host_label)}
          </span>
          <span className="min-w-0 truncate font-mono" data-testid="local-browser-hostname">
            {data?.hostname || "—"}
          </span>
        </div>

        {/* Current location + the two jumps. Both nav buttons disable on an
            empty server answer rather than guessing: `parent === ""` is the
            contract's "no further up", and `home === ""` means there is no
            meaningful home to offer. */}
        <div className="flex items-center gap-2">
          <div className="min-w-0 flex-1 rounded-md bg-muted px-2.5 py-1.5 font-mono text-micro text-muted-foreground break-all">
            {data?.path || "—"}
          </div>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="h-7 shrink-0 text-caption"
            disabled={!data?.parent || saving}
            onClick={goUp}
          >
            <ArrowUp className="size-3" />
            {t(($) => $.resources.local_browser_up)}
          </Button>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="h-7 shrink-0 text-caption"
            disabled={!data?.home || saving}
            onClick={goHome}
          >
            <Home className="size-3" />
            {t(($) => $.resources.local_browser_home)}
          </Button>
        </div>

        {forbidden ? (
          <StateBanner text={t(($) => $.resources.local_browser_forbidden)} />
        ) : browseReason ? (
          <StateBanner text={browseReason} />
        ) : isPending && !data ? (
          <div className="flex items-center justify-center gap-2 py-6 text-caption text-muted-foreground">
            <Spinner className="size-4" />
            {t(($) => $.resources.local_browser_loading)}
          </div>
        ) : (
          <div className="max-h-64 space-y-1 overflow-y-auto rounded-md border p-1">
            {(data?.dirs.length ?? 0) === 0 && (
              <p className="py-3 text-center text-caption text-muted-foreground">
                {t(($) => $.resources.local_browser_empty)}
              </p>
            )}
            {data?.dirs.map((dir) => (
              <DirRow key={dir.path} dir={dir} onEnter={goTo} />
            ))}
          </div>
        )}

        {/* Manual entry doubles as the recovery path for a failed listing:
            the input keeps its text, so fixing a typo is a re-submit. */}
        <form
          onSubmit={(e) => {
            e.preventDefault();
            goToInput();
          }}
          className="flex items-center gap-1.5"
        >
          <input
            type="text"
            value={pathInput}
            onChange={(e) => setPathInput(e.target.value)}
            aria-label={t(($) => $.resources.local_browser_path_aria)}
            placeholder={t(($) => $.resources.local_browser_path_placeholder)}
            className="h-8 min-w-0 flex-1 rounded-md border bg-transparent px-2 text-caption outline-none placeholder:text-muted-foreground focus-visible:ring-1 focus-visible:ring-ring"
          />
          <Button
            type="submit"
            size="sm"
            variant="outline"
            className="h-7 shrink-0 text-caption"
            disabled={!pathInput.trim() || saving}
          >
            {t(($) => $.resources.local_browser_goto)}
          </Button>
        </form>

        {/* Non-resolved daemon: browsing stays useful (it is how someone
            finds the right path), saving does not. */}
        {data && !daemonResolved && !forbidden && !browseReason && !isPending && (
          <StateBanner
            tone="warning"
            text={t(($) => $.resources.local_browser_no_daemon)}
          />
        )}

        <div className="space-y-2">
          <div className="text-caption font-medium text-muted-foreground">
            {t(($) => $.resources.local_browser_mode_heading)}
          </div>
          <LocalDirectoryModeOptions
            value={mode}
            onChange={setMode}
            unavailableReason={unavailableReason}
          />
        </div>

        {errorMessage && <StateBanner text={errorMessage} />}

        <DialogFooter>
          <Button
            type="button"
            variant="ghost"
            onClick={() => onOpenChange(false)}
            disabled={saving}
          >
            {t(($) => $.resources.mode_cancel)}
          </Button>
          <Button
            type="button"
            onClick={confirm}
            disabled={!daemonResolved || saving}
          >
            {confirmLabel}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function StateBanner({
  text,
  tone = "error",
}: {
  text: string;
  tone?: "error" | "warning";
}) {
  return (
    <div
      className={`flex items-start gap-2 rounded-md border px-3 py-2 text-caption ${
        tone === "warning"
          ? "border-warning/30 bg-warning/5 text-warning"
          : "border-destructive/30 bg-destructive/5 text-destructive"
      }`}
    >
      <TriangleAlert className="size-3.5 mt-0.5 shrink-0" />
      <span>{text}</span>
    </div>
  );
}

interface DirRowProps {
  dir: LocalDirEntry;
  onEnter: (path: string) => void;
}

function DirRow({ dir, onEnter }: DirRowProps) {
  const { t } = useT("projects");
  // aria-disabled rather than disabled: browsers suppress pointer events on
  // disabled controls, which would take the tooltip's only trigger with them
  // — and the point of blocking a system path is to be able to say so.
  const blocked = dir.blocked;
  return (
    <button
      type="button"
      aria-disabled={blocked}
      aria-label={dir.name}
      onClick={() => {
        if (blocked) return;
        onEnter(dir.path);
      }}
      className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-caption transition-colors hover:bg-accent aria-disabled:cursor-not-allowed aria-disabled:opacity-50 aria-disabled:hover:bg-transparent"
    >
      {blocked ? (
        <Folder className="size-3.5 shrink-0 text-muted-foreground" />
      ) : (
        <FolderOpen className="size-3.5 shrink-0 text-muted-foreground" />
      )}
      <span className="min-w-0 flex-1 truncate">{dir.name || dir.path}</span>
      {blocked ? (
        <Tooltip>
          <TooltipTrigger
            render={
              <Badge variant="secondary" className="shrink-0 font-normal">
                {t(($) => $.resources.local_browser_blocked_badge)}
              </Badge>
            }
          />
          <TooltipContent side="top">
            {t(($) => $.resources.local_browser_blocked_tooltip)}
          </TooltipContent>
        </Tooltip>
      ) : (
        // The affordance, not the requirement: a row is enterable either way,
        // this just promises the list inside is not empty.
        dir.has_children && (
          <ChevronRight className="size-3.5 shrink-0 text-muted-foreground" />
        )
      )}
    </button>
  );
}
