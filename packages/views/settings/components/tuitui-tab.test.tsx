// @vitest-environment jsdom

import { type ReactNode } from "react";
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import { configStore } from "@multica/core/config";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";

const membersRef = vi.hoisted(() => ({
  current: [{ user_id: "user-1", role: "owner" as "owner" | "admin" | "member" }],
}));
const agentsRef = vi.hoisted(() => ({
  current: [
    { id: "agent-1", name: "Aria" },
    { id: "agent-7", name: "Bolt" },
  ],
}));
const installationsRef = vi.hoisted(() => ({
  current: {
    installations: [] as Array<Record<string, unknown>>,
    configured: true,
  },
}));
const groupsRef = vi.hoisted(() => ({
  current: {
    data: {
      groups: [],
      group_discovery_supported: false,
    } as {
      groups: unknown[];
      group_discovery_supported: boolean;
      bot_identities?: Record<string, unknown>;
    },
    isLoading: false,
    isError: false,
  },
}));
// One shared mutation controller so each test can assert submit guard + call
// args, and drive isPending for the in-flight states the panel reads.
const mutationsRef = vi.hoisted(() => ({
  register: { isPending: false, mutateAsync: vi.fn() },
  disconnect: { isPending: false, mutateAsync: vi.fn() },
  forget: { isPending: false, mutateAsync: vi.fn() },
}));
const toastRef = vi.hoisted(() => ({
  success: vi.fn(),
  error: vi.fn(),
  message: vi.fn(),
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: (opts: { queryKey: readonly unknown[]; enabled?: boolean }) => {
    if (opts.enabled === false) return { data: undefined, isLoading: false };
    const key = JSON.stringify(opts.queryKey);
    if (key.includes("members")) return { data: membersRef.current };
    if (key.includes("agents")) return { data: agentsRef.current };
    if (key.includes("groups")) return groupsRef.current;
    if (key.includes("installations"))
      return { data: installationsRef.current, isLoading: false };
    return { data: undefined, isLoading: false };
  },
  useQueryClient: () => ({ invalidateQueries: vi.fn() }),
  queryOptions: <T,>(opts: T) => opts,
}));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));

vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({ queryKey: ["members"], queryFn: vi.fn() }),
  agentListOptions: () => ({ queryKey: ["agents"], queryFn: vi.fn() }),
}));

vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({
    getAgentName: (agentId: string) => `Agent ${agentId}`,
    getMemberName: () => "Member",
    getSquadName: () => "Squad",
    getActorName: () => "Actor",
    getActorInitials: () => "A",
    getActorAvatarUrl: () => null,
  }),
}));

vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: ({ actorId }: { actorId: string }) => (
    <span data-testid="actor-avatar" data-actor-id={actorId} />
  ),
}));

vi.mock("@multica/core/auth", () => {
  const useAuthStore = Object.assign(
    (selector?: (s: { user: { id: string } }) => unknown) =>
      selector ? selector({ user: { id: "user-1" } }) : { user: { id: "user-1" } },
    { getState: () => ({ user: { id: "user-1" } }) },
  );
  return { useAuthStore };
});

vi.mock("@multica/core/tuitui", () => ({
  tuituiInstallationsOptions: (wsId: string) => ({
    queryKey: ["tuitui", wsId, "installations"],
    queryFn: vi.fn(),
  }),
  tuituiGroupsOptions: (wsId: string) => ({
    queryKey: ["tuitui", wsId, "groups"],
    queryFn: vi.fn(),
  }),
  useRegisterTuituiBYO: () => mutationsRef.register,
  useDeleteTuituiInstallation: () => mutationsRef.disconnect,
  useForgetTuituiGroup: () => mutationsRef.forget,
}));

vi.mock("sonner", () => ({ toast: toastRef }));

import { TuituiAgentBindButton, TuituiTab } from "./tuitui-tab";

const TEST_RESOURCES = { en: { common: enCommon, settings: enSettings } };

afterEach(cleanup);

function renderUI(children: ReactNode) {
  return render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      {children}
    </I18nProvider>,
  );
}

async function pickAgent(name: string) {
  await userEvent.click(screen.getByRole("combobox", { name: "Agent" }));
  await userEvent.click(await screen.findByRole("option", { name }));
}

function resetFixtures() {
  vi.clearAllMocks();
  membersRef.current = [{ user_id: "user-1", role: "owner" }];
  agentsRef.current = [
    { id: "agent-1", name: "Aria" },
    { id: "agent-7", name: "Bolt" },
  ];
  installationsRef.current = { installations: [], configured: true };
  groupsRef.current = {
    data: { groups: [], group_discovery_supported: false },
    isLoading: false,
    isError: false,
  };
  mutationsRef.register = { isPending: false, mutateAsync: vi.fn() };
  mutationsRef.disconnect = { isPending: false, mutateAsync: vi.fn() };
  mutationsRef.forget = { isPending: false, mutateAsync: vi.fn() };
  configStore.getState().setTuituiSupported(true);
}

describe("TuituiTab", () => {
  beforeEach(resetFixtures);

  it("renders the unavailable state and hides install entry points when not configured", () => {
    installationsRef.current = { installations: [], configured: false };
    renderUI(<TuituiTab />);
    expect(
      screen.getByText("Tuitui integration is currently unavailable"),
    ).toBeInTheDocument();
    expect(screen.queryByTestId("tuitui-add-installation")).toBeNull();
  });

  it("lists no bots and shows the add affordance when configured with zero installations", () => {
    renderUI(<TuituiTab />);
    expect(screen.getByText("No bots connected yet")).toBeInTheDocument();
    expect(screen.getByTestId("tuitui-add-installation")).toBeInTheDocument();
  });

  it("hides the add affordance for a member without manage rights", () => {
    membersRef.current = [{ user_id: "user-1", role: "member" }];
    renderUI(<TuituiTab />);
    expect(screen.queryByTestId("tuitui-add-installation")).toBeNull();
  });

  it("blocks BYO submit until the server address and both credentials are non-empty", async () => {
    renderUI(<TuituiTab />);
    await userEvent.click(screen.getByTestId("tuitui-add-installation"));
    await pickAgent("Aria");
    const submit = screen.getByTestId("tuitui-add-submit");
    expect(submit).toBeDisabled();
    await userEvent.type(screen.getByTestId("tuitui-byo-app-id"), "app-1");
    await userEvent.type(screen.getByTestId("tuitui-byo-app-secret"), "secret-1");
    // Credentials alone are not enough: an empty address must never fall
    // through to the server's default target.
    expect(submit).toBeDisabled();
    await userEvent.type(screen.getByTestId("tuitui-byo-base-url"), "  ");
    // Whitespace only: trimmed value is empty, still blocked.
    expect(submit).toBeDisabled();
    await userEvent.clear(screen.getByTestId("tuitui-byo-base-url"));
    await userEvent.type(screen.getByTestId("tuitui-byo-base-url"), "https://tt.test:8443");
    await waitFor(() => expect(submit).toBeEnabled());
  });

  it("submits trimmed credentials and a chosen agent through the mutation", async () => {
    mutationsRef.register.mutateAsync.mockResolvedValue({ id: "i1", status: "active" });
    renderUI(<TuituiTab />);
    await userEvent.click(screen.getByTestId("tuitui-add-installation"));
    await pickAgent("Bolt");
    await userEvent.type(screen.getByTestId("tuitui-byo-base-url"), "  https://tt.test:8443  ");
    await userEvent.type(screen.getByTestId("tuitui-byo-app-id"), "  app-7  ");
    await userEvent.type(screen.getByTestId("tuitui-byo-app-secret"), "sec-7");
    await userEvent.click(screen.getByTestId("tuitui-add-submit"));
    await waitFor(() =>
      expect(mutationsRef.register.mutateAsync).toHaveBeenCalledWith({
        agentId: "agent-7",
        base_url: "https://tt.test:8443",
        app_id: "app-7",
        app_secret: "sec-7",
      }),
    );
    expect(toastRef.success).toHaveBeenCalled();
  });

  it("surfaces a BYO failure as a toast without claiming success", async () => {
    mutationsRef.register.mutateAsync.mockRejectedValue(new Error("invalid credentials"));
    renderUI(<TuituiTab />);
    await userEvent.click(screen.getByTestId("tuitui-add-installation"));
    await pickAgent("Aria");
    await userEvent.type(screen.getByTestId("tuitui-byo-base-url"), "https://tt.test");
    await userEvent.type(screen.getByTestId("tuitui-byo-app-id"), "app-1");
    await userEvent.type(screen.getByTestId("tuitui-byo-app-secret"), "sec-1");
    await userEvent.click(screen.getByTestId("tuitui-add-submit"));
    await waitFor(() =>
      expect(toastRef.error).toHaveBeenCalledWith("invalid credentials"),
    );
    expect(toastRef.success).not.toHaveBeenCalled();
  });

  it("shows a server address rejection inline beside the visible text address field", async () => {
    mutationsRef.register.mutateAsync.mockRejectedValue(
      new Error('invalid server address: a path is not allowed — remove "/robot"'),
    );
    renderUI(<TuituiTab />);
    await userEvent.click(screen.getByTestId("tuitui-add-installation"));
    await pickAgent("Aria");
    const address = screen.getByTestId("tuitui-byo-base-url");
    // The address is a hostname, not a secret: visible text input with the
    // canonical scheme-bearing placeholder.
    expect(address.getAttribute("type")).toBe("text");
    expect(address.getAttribute("placeholder")).toBe("https://tuitui.internal:8282");
    await userEvent.type(address, "https://tt.test/robot");
    await userEvent.type(screen.getByTestId("tuitui-byo-app-id"), "app-1");
    await userEvent.type(screen.getByTestId("tuitui-byo-app-secret"), "sec-1");
    await userEvent.click(screen.getByTestId("tuitui-add-submit"));
    const alert = await screen.findByTestId("tuitui-byo-error");
    expect(alert).toHaveTextContent("a path is not allowed");
    expect(address).toHaveAttribute("aria-invalid", "true");
    // The dialog stays open so the user can fix the address and retry.
    expect(screen.getByTestId("tuitui-add-submit")).toBeEnabled();
    await userEvent.type(
      screen.getByTestId("tuitui-byo-app-id"),
      "x",
    );
    // Any field edit clears the stale rejection.
    expect(screen.queryByTestId("tuitui-byo-error")).toBeNull();
    expect(address).toHaveAttribute("aria-invalid", "false");
  });

  it("masks both credential inputs as password fields", async () => {
    renderUI(<TuituiTab />);
    await userEvent.click(screen.getByTestId("tuitui-add-installation"));
    expect(screen.getByTestId("tuitui-byo-app-id").getAttribute("type")).toBe("password");
    expect(screen.getByTestId("tuitui-byo-app-secret").getAttribute("type")).toBe("password");
  });

  it("requires confirmation before revoking an installation", async () => {
    mutationsRef.disconnect.mutateAsync.mockResolvedValue(undefined);
    installationsRef.current = {
      configured: true,
      installations: [
        { id: "i1", agent_id: "agent-1", status: "active", installed_at: "" },
      ],
    };
    renderUI(<TuituiTab />);
    await userEvent.click(screen.getByTestId("tuitui-installation-disconnect"));
    // The AlertDialog gates the destructive action; nothing is sent until confirm.
    expect(mutationsRef.disconnect.mutateAsync).not.toHaveBeenCalled();
    await userEvent.click(await screen.findByTestId("tuitui-disconnect-confirm"));
    await waitFor(() =>
      expect(mutationsRef.disconnect.mutateAsync).toHaveBeenCalledWith("i1"),
    );
  });
});

describe("TuituiAgentBindButton", () => {
  beforeEach(resetFixtures);

  it("renders nothing when the deployment flag is off", () => {
    configStore.getState().setTuituiSupported(false);
    const { container } = renderUI(
      <TuituiAgentBindButton agentId="agent-1" agentName="Aria" agentOwnerId="user-1" />,
    );
    expect(container.firstChild).toBeNull();
  });

  it("renders nothing for a user without workspace rights", () => {
    membersRef.current = [{ user_id: "user-1", role: "member" }];
    const { container } = renderUI(
      <TuituiAgentBindButton agentId="agent-1" agentOwnerId="someone-else" />,
    );
    expect(container.firstChild).toBeNull();
  });

  it("shows the connect CTA for an unbound, manageable agent", () => {
    renderUI(
      <TuituiAgentBindButton
        agentId="agent-1"
        agentName="Aria"
        agentOwnerId="user-1"
      />,
    );
    expect(screen.getByTestId("tuitui-agent-connect")).toBeInTheDocument();
  });

  it("shows the connected badge with a disconnect instead of the CTA once installed", () => {
    installationsRef.current = {
      configured: true,
      installations: [{ id: "i1", agent_id: "agent-1", status: "active" }],
    };
    renderUI(
      <TuituiAgentBindButton agentId="agent-1" agentName="Aria" agentOwnerId="user-1" />,
    );
    expect(screen.getByTestId("tuitui-agent-bot-connected")).toBeInTheDocument();
    expect(screen.getByTestId("tuitui-agent-bot-disconnect")).toBeInTheDocument();
    expect(screen.queryByTestId("tuitui-agent-connect")).toBeNull();
  });

  it("hides the CTA when the integration is not configured on the server", () => {
    installationsRef.current = { configured: false, installations: [] };
    const { container } = renderUI(
      <TuituiAgentBindButton agentId="agent-1" agentName="Aria" agentOwnerId="user-1" />,
    );
    expect(screen.queryByTestId("tuitui-agent-connect")).toBeNull();
    expect(container.firstChild).toBeNull();
  });
});
