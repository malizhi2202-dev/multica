// @vitest-environment jsdom

import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { LocalDirBrowseResponseSchema } from "@multica/core/api/schemas";
import { renderWithI18n } from "../../test/i18n";
import en from "../../locales/en/projects.json";
import {
  LocalDirectoryBrowserDialog,
  type LocalDirectoryBrowserSelection,
} from "./local-directory-browser-dialog";

const strings = en.resources;

// The documented 200 shape of GET /api/workspaces/{id}/local-dirs.
const RESOLVED = {
  path: "/home/malizhi/project",
  parent: "/home/malizhi",
  hostname: "77220d771381",
  home: "/home/malizhi",
  daemon_id: "01a0c304-922a-7656-b761-20cbefa307b3",
  daemon_status: "resolved",
  dirs: [
    {
      name: "multica",
      path: "/home/malizhi/project/multica",
      has_children: true,
      blocked: false,
    },
    { name: "etc", path: "/etc", has_children: true, blocked: true },
  ],
};

// What the mocked useQuery hands the dialog. Tests overwrite `data`/`error`
// to drive the loading, forbidden, and daemon-unresolved states.
let queryState: {
  data: unknown;
  error: unknown;
  isPending: boolean;
} = { data: RESOLVED, error: null, isPending: false };

// Params of every localDirBrowseOptions() call — navigation is asserted by
// reading them, which also proves the query key follows the path.
let browseParams: Array<{ path?: string }> = [];

vi.mock("@tanstack/react-query", () => ({
  useQuery: () => queryState,
  queryOptions: (options: unknown) => options,
}));

vi.mock("@multica/core/projects", () => ({
  localDirBrowseOptions: (wsId: string, params: { path?: string } = {}) => {
    browseParams.push(params);
    return {
      queryKey: ["local-dirs", wsId, params.path ?? null],
      queryFn: vi.fn(),
    };
  },
}));

vi.mock("@multica/core/config", () => ({
  useConfigStore: (selector: (state: { localWorktreeSupported: boolean }) => unknown) =>
    selector({ localWorktreeSupported: true }),
}));

vi.mock("@multica/core/api", () => {
  class ApiError extends Error {
    readonly status: number;
    constructor(message: string, status: number) {
      super(message);
      this.name = "ApiError";
      this.status = status;
    }
  }
  return {
    ApiError,
    // Mirrors the real gate: 4xx messages are written for users, 5xx are
    // internals that must never reach them.
    clientErrorMessage: (err: unknown) =>
      err instanceof ApiError && err.status >= 400 && err.status < 500
        ? err.message
        : undefined,
  };
});

import { ApiError } from "@multica/core/api";

function renderDialog(
  overrides: {
    onConfirm?: (selection: LocalDirectoryBrowserSelection) => void;
    errorMessage?: string;
  } = {},
) {
  const onConfirm = overrides.onConfirm ?? vi.fn();
  renderWithI18n(
    <LocalDirectoryBrowserDialog
      open
      onOpenChange={() => {}}
      wsId="workspace-1"
      confirmLabel="Add folder"
      errorMessage={overrides.errorMessage}
      onConfirm={onConfirm}
    />,
  );
  return { onConfirm };
}

beforeEach(() => {
  queryState = { data: RESOLVED, error: null, isPending: false };
  browseParams = [];
});

describe("LocalDirectoryBrowserDialog — listing", () => {
  it("names the machine and the directory, so nobody reads it as their own disk", () => {
    renderDialog();
    expect(screen.getByTestId("local-browser-hostname").textContent).toBe(
      "77220d771381",
    );
    expect(screen.getByText("/home/malizhi/project")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "multica" })).toBeInTheDocument();
  });

  it("marks a blocked entry as protected and refuses to enter it", () => {
    renderDialog();
    const blocked = screen.getByRole("button", { name: "etc" });
    expect(blocked.getAttribute("aria-disabled")).toBe("true");
    expect(screen.getByText(strings.local_browser_blocked_badge)).toBeInTheDocument();

    fireEvent.click(blocked);
    // Only the initial (no-path) listing was ever requested.
    expect(browseParams).toEqual([{}]);
  });

  it("navigates into a child, up to the parent, and home", () => {
    renderDialog();
    fireEvent.click(screen.getByRole("button", { name: "multica" }));
    expect(browseParams.at(-1)).toEqual({ path: "/home/malizhi/project/multica" });

    fireEvent.click(screen.getByRole("button", { name: strings.local_browser_up }));
    expect(browseParams.at(-1)).toEqual({ path: "/home/malizhi" });

    fireEvent.click(screen.getByRole("button", { name: strings.local_browser_home }));
    expect(browseParams.at(-1)).toEqual({ path: "/home/malizhi" });
  });

  it("disables Up when the server says there is no parent to offer", () => {
    queryState = { data: { ...RESOLVED, parent: "" }, error: null, isPending: false };
    renderDialog();
    expect(
      screen.getByRole("button", { name: strings.local_browser_up }),
    ).toBeDisabled();
  });
});

describe("LocalDirectoryBrowserDialog — manual path", () => {
  it("validates and enters a typed absolute path", () => {
    renderDialog();
    const input = screen.getByLabelText(strings.local_browser_path_aria);
    fireEvent.change(input, { target: { value: "/srv/data" } });
    fireEvent.click(screen.getByRole("button", { name: strings.local_browser_goto }));
    expect(browseParams.at(-1)).toEqual({ path: "/srv/data" });
  });

  it("surfaces the backend's own reason when a path is rejected", () => {
    queryState = {
      data: undefined,
      error: new ApiError("not_absolute", 400, "Bad Request"),
      isPending: false,
    };
    renderDialog();
    expect(screen.getByText("not_absolute")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Add folder" }),
    ).toBeDisabled();
  });
});

describe("LocalDirectoryBrowserDialog — permission and daemon states", () => {
  it("explains a 403 instead of listing anything", () => {
    queryState = {
      data: undefined,
      error: new ApiError("only owners and admins may browse", 403, "Forbidden"),
      isPending: false,
    };
    renderDialog();
    expect(screen.getByText(strings.local_browser_forbidden)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "multica" })).toBeNull();
  });

  it.each(["none", "ambiguous"])("blocks saving when daemon_status is %s", (status) => {
    queryState = {
      data: { ...RESOLVED, daemon_status: status, daemon_id: "" },
      error: null,
      isPending: false,
    };
    renderDialog();
    expect(screen.getByText(strings.local_browser_no_daemon)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add folder" })).toBeDisabled();
    // Browsing stays useful — this state is how you find a servable path.
    expect(screen.getByRole("button", { name: "multica" })).toBeEnabled();
  });

  // The API boundary is what degrades an unknown status; the dialog must
  // render the downgraded answer without crashing. Parsing through the real
  // schema keeps the two halves honest.
  it("renders an unknown daemon_status as the safe ambiguous state", () => {
    const parsed = LocalDirBrowseResponseSchema.parse({
      ...RESOLVED,
      daemon_status: "resolved_by_hint",
    });
    queryState = { data: parsed, error: null, isPending: false };
    renderDialog();
    expect(screen.getByText(strings.local_browser_no_daemon)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add folder" })).toBeDisabled();
  });

  it("survives a schema-fallback response and offers nothing to save", () => {
    // EMPTY_LOCAL_DIR_BROWSE_RESPONSE-shaped fallback: everything blank.
    queryState = {
      data: {
        path: "",
        parent: "",
        hostname: "",
        home: "",
        daemon_id: "",
        daemon_status: "ambiguous",
        dirs: [],
      },
      error: null,
      isPending: false,
    };
    renderDialog();
    expect(screen.getByRole("button", { name: "Add folder" })).toBeDisabled();
    expect(screen.getByText(strings.local_browser_empty)).toBeInTheDocument();
  });
});

describe("LocalDirectoryBrowserDialog — confirm", () => {
  it("submits the server-resolved daemon id with the picked path and mode", () => {
    const { onConfirm } = renderDialog();
    const worktree = screen.getAllByRole("radio")[1];
    expect(worktree).toBeDefined();
    fireEvent.click(worktree!);
    fireEvent.click(screen.getByRole("button", { name: "Add folder" }));

    expect(onConfirm).toHaveBeenCalledWith({
      localPath: "/home/malizhi/project",
      daemonId: "01a0c304-922a-7656-b761-20cbefa307b3",
      label: "project",
      mode: "worktree",
    });
  });

  it("keeps the create rejection visible without closing the dialog", async () => {
    const { onConfirm } = renderDialog({ errorMessage: "this daemon already has a directory" });
    fireEvent.click(screen.getByRole("button", { name: "Add folder" }));
    await waitFor(() => expect(onConfirm).toHaveBeenCalledTimes(1));
    expect(screen.getByText("this daemon already has a directory")).toBeInTheDocument();
  });
});
