// @vitest-environment jsdom

import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { renderWithI18n } from "../../test/i18n";
import en from "../../locales/en/projects.json";

const strings = en.resources;

// The documented browse response; the daemon id below is deliberately
// different from the desktop id so the test can prove which one is submitted.
const BROWSE = {
  path: "/srv/work/game-client",
  parent: "/srv/work",
  hostname: "deploy-77",
  home: "/home/deploy",
  daemon_id: "01a0c304-922a-7656-b761-20cbefa307b3",
  daemon_status: "resolved",
  dirs: [
    {
      name: "game-client",
      path: "/srv/work/game-client",
      has_children: false,
      blocked: false,
    },
  ],
};

let desktopShell = false;
let serverBrowserSupported = true;
let localDaemon: {
  daemonId: string | null;
  deviceName: string | null;
  running: boolean;
} = { daemonId: null, deviceName: null, running: false };

const createMock = vi.fn().mockResolvedValue({ id: "res-new" });
const validateMock = vi.fn();

vi.mock("@tanstack/react-query", () => ({
  useQuery: (options: { queryKey?: unknown[] }) => {
    const key = options?.queryKey?.[0];
    if (key === "local-dirs")
      return { data: BROWSE, error: null, isPending: false };
    return { data: [], error: null, isPending: false };
  },
  queryOptions: (options: unknown) => options,
}));

vi.mock("@multica/core/projects", () => ({
  projectResourcesOptions: () => ({ queryKey: ["project-resources"], queryFn: vi.fn() }),
  // Shape mirrors the real factory: key carries wsId + path so the mocked
  // useQuery above can tell the browse query apart from the resource list.
  localDirBrowseOptions: (wsId: string, params: { path?: string } = {}) => ({
    queryKey: ["local-dirs", wsId, params.path ?? null],
    queryFn: vi.fn(),
  }),
  useCreateProjectResource: () => ({ mutateAsync: createMock, isPending: false }),
  useUpdateProjectResource: () => ({ mutateAsync: vi.fn(), isPending: false }),
  useDeleteProjectResource: () => ({ mutateAsync: vi.fn(), isPending: false }),
}));

vi.mock("@multica/core/config", () => ({
  useConfigStore: (
    selector: (state: {
      localWorktreeSupported: boolean;
      localDirBrowserSupported: boolean;
    }) => unknown,
  ) =>
    selector({
      localWorktreeSupported: true,
      localDirBrowserSupported: serverBrowserSupported,
    }),
}));

vi.mock("@multica/core/runtimes", () => ({
  runtimeListOptions: () => ({ queryKey: ["runtimes"], queryFn: vi.fn() }),
  runtimeAdvertisesLocalWorktree: () => true,
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace-1" }));
vi.mock("@multica/core/paths", () => ({
  useCurrentWorkspace: () => ({ id: "workspace-1", slug: "ws", repos: [] }),
}));
// The real capability resolver stays real — this file is precisely about the
// wiring that feeds it. Only the preload bridges are faked.
vi.mock("../../platform/local-directory", async (importOriginal) => ({
  ...(await importOriginal<Record<string, unknown>>()),
  isDesktopShell: () => desktopShell,
  pickDirectory: vi.fn(),
  validateLocalDirectory: () => validateMock(),
}));
vi.mock("../../platform/use-local-daemon-status", () => ({
  useLocalDaemonStatus: () => localDaemon,
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("@multica/core/api", () => {
  class ApiError extends Error {
    readonly status: number;
    constructor(message: string, status: number) {
      super(message);
      this.name = "ApiError";
      this.status = status;
    }
  }
  return { ApiError, clientErrorMessage: () => undefined };
});

import { ProjectResourcesSection } from "./project-resources-section";

beforeEach(() => {
  createMock.mockClear();
  validateMock.mockReset();
  desktopShell = false;
  serverBrowserSupported = true;
  localDaemon = { daemonId: null, deviceName: null, running: false };
});

describe("ProjectResourcesSection — capability tiers", () => {
  it("offers the server browse entry when the server declares it", () => {
    renderWithI18n(<ProjectResourcesSection projectId="p1" />);
    expect(
      screen.getByRole("button", { name: strings.add_server_directory_button }),
    ).toBeInTheDocument();
    expect(screen.queryByText(strings.local_read_only_hint)).toBeNull();
  });

  it("falls to a read-only hint on web when the server lacks the capability", () => {
    serverBrowserSupported = false;
    renderWithI18n(<ProjectResourcesSection projectId="p1" />);
    expect(
      screen.queryByRole("button", { name: strings.add_server_directory_button }),
    ).toBeNull();
    expect(screen.getByText(strings.local_read_only_hint)).toBeInTheDocument();
  });

  it("creates the resource with the SERVER-resolved daemon id, not a browser-local one", async () => {
    renderWithI18n(<ProjectResourcesSection projectId="p1" />);
    fireEvent.click(
      screen.getByRole("button", { name: strings.add_server_directory_button }),
    );
    // The dialog renders portaled; the listing proves it is the open one.
    expect(await screen.findByText("/srv/work/game-client")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: strings.mode_add }));
    await waitFor(() => expect(createMock).toHaveBeenCalledTimes(1));
    expect(createMock).toHaveBeenCalledWith({
      resource_type: "local_directory",
      resource_ref: {
        local_path: "/srv/work/game-client",
        daemon_id: BROWSE.daemon_id,
        label: "game-client",
        execution_mode: "in_place",
      },
    });
  });

  it("keeps the desktop picker affordance when the native bridge exists", () => {
    desktopShell = true;
    localDaemon = { daemonId: "daemon-1", deviceName: "Mac", running: true };
    renderWithI18n(<ProjectResourcesSection projectId="p1" />);
    expect(
      screen.getByRole("button", { name: strings.add_local_directory_button }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: strings.add_server_directory_button }),
    ).toBeNull();
  });

  // Manual tier: a locally registered daemon without the native picker. The
  // typed path goes through the same mode dialog, so the mode choice and its
  // server-side rejection surface the desktop flow already exercises.
  it("types a manual path, chooses a mode, and pins it to the local daemon", async () => {
    serverBrowserSupported = false;
    localDaemon = { daemonId: "daemon-local", deviceName: "Old Mac", running: true };
    validateMock.mockResolvedValue({ ok: true, is_git_repo: undefined });

    renderWithI18n(<ProjectResourcesSection projectId="p1" />);
    fireEvent.change(screen.getByLabelText(strings.local_manual_path_aria), {
      target: { value: "/Users/dev/work/game-client" },
    });
    fireEvent.click(screen.getByRole("button", { name: strings.url_submit }));

    expect(await screen.findByText(strings.mode_dialog_title)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: strings.mode_add }));
    await waitFor(() => expect(createMock).toHaveBeenCalledTimes(1));
    expect(createMock).toHaveBeenCalledWith({
      resource_type: "local_directory",
      resource_ref: {
        local_path: "/Users/dev/work/game-client",
        daemon_id: "daemon-local",
        label: "game-client",
        execution_mode: "in_place",
      },
    });
  });
});
