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
// Two pages, chosen by the offset in the query key, so paging and the moment the
// affordance disappears are both observable.
type TuituiRejectionsFixture =
  | { rejections: Array<Record<string, unknown>>; next_offset?: number }
  | undefined;
const rejectionsRef = vi.hoisted(() => ({
  current: undefined as TuituiRejectionsFixture,
  next: undefined as TuituiRejectionsFixture,
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
  allowlist: { isPending: false, mutateAsync: vi.fn() },
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
    if (key.includes("rejections")) {
      const offset = opts.queryKey[opts.queryKey.length - 1];
      return {
        data: offset === 0 ? rejectionsRef.current : rejectionsRef.next,
        isLoading: false,
      };
    }
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
  tuituiRejectionsOptions: (wsId: string, installationId: string, offset: number) => ({
    queryKey: ["tuitui", wsId, "rejections", installationId, offset],
    queryFn: vi.fn(),
  }),
  useRegisterTuituiBYO: () => mutationsRef.register,
  useDeleteTuituiInstallation: () => mutationsRef.disconnect,
  useForgetTuituiGroup: () => mutationsRef.forget,
  useUpdateTuituiAllowlists: () => mutationsRef.allowlist,
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
  mutationsRef.allowlist = { isPending: false, mutateAsync: vi.fn() };
  rejectionsRef.current = undefined;
  rejectionsRef.next = undefined;
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

  it("summarizes an in-force allowlist on the row, and never an open one", () => {
    installationsRef.current = {
      configured: true,
      installations: [
        {
          id: "i1",
          agent_id: "agent-1",
          status: "active",
          installed_at: "",
          allow_from_mode: "allowlist",
          allow_from: ["alice"],
          group_allow_from_mode: "unrestricted",
          group_allow_from: [],
        },
      ],
    };
    renderUI(<TuituiTab />);
    // Which gate is in force has to be readable without opening anything: an
    // empty list and an untouched one look the same everywhere the mode is not
    // shown beside the entries.
    expect(screen.getByTestId("tuitui-installation-allowlist")).toHaveTextContent(
      "1 account can DM it",
    );
    // The open side is stated too, so the pair cannot be misread as "nothing is
    // restricted" when exactly one gate is closed.
    expect(screen.getByTestId("tuitui-installation-allowlist")).toHaveTextContent(
      "Any group can trigger it",
    );
    expect(screen.queryByTestId("tuitui-allowlist-direct-closed")).toBeNull();
  });

  it("flags a bot whose cleared list is refusing everyone", () => {
    installationsRef.current = {
      configured: true,
      installations: [
        {
          id: "i1",
          agent_id: "agent-1",
          status: "active",
          installed_at: "",
          allow_from_mode: "allowlist",
          allow_from: [],
          group_allow_from_mode: "allowlist",
          group_allow_from: ["778899"],
        },
      ],
    };
    renderUI(<TuituiTab />);
    expect(screen.getByTestId("tuitui-allowlist-direct-closed")).toHaveTextContent(
      "This box is empty: the bot is refusing every direct message.",
    );
    expect(screen.getByTestId("tuitui-allowlist-group-listed")).toHaveTextContent(
      "1 group allowed",
    );
  });

  it("hides the allowlist entry point for a revoked bot and a member", async () => {
    installationsRef.current = {
      configured: true,
      installations: [
        { id: "i1", agent_id: "agent-1", status: "revoked", installed_at: "" },
      ],
    };
    renderUI(<TuituiTab />);
    // A revoked bot is not connected, so its gate decides nothing and the write
    // would be refused.
    expect(screen.queryByTestId("tuitui-installation-allowlist-edit")).toBeNull();

    cleanup();
    membersRef.current = [{ user_id: "user-1", role: "member" }];
    installationsRef.current = {
      configured: true,
      installations: [{ id: "i1", agent_id: "agent-1", status: "active", installed_at: "" }],
    };
    renderUI(<TuituiTab />);
    expect(screen.queryByTestId("tuitui-installation-allowlist-edit")).toBeNull();
  });

  it("seeds the dialog from the stored gate and reads an unset one as the wildcard", async () => {
    installationsRef.current = {
      configured: true,
      installations: [
        {
          id: "i1",
          agent_id: "agent-1",
          status: "active",
          installed_at: "",
          allow_from_mode: "allowlist",
          allow_from: ["alice", "bob"],
          group_allow_from_mode: "unrestricted",
          group_allow_from: [],
        },
      ],
    };
    renderUI(<TuituiTab />);
    await userEvent.click(screen.getByTestId("tuitui-installation-allowlist-edit"));
    const dialog = await screen.findByTestId("tuitui-allowlist-dialog");
    expect(dialog).toBeInTheDocument();
    // A stored list is edited as itself…
    expect(screen.getByTestId("tuitui-allowlist-direct")).toHaveValue("alice, bob");
    // …and a gate nobody has ever configured starts open, shown as `*` rather
    // than an empty box that would read as "restricted to nobody" — and, once
    // saved, mean exactly that.
    expect(screen.getByTestId("tuitui-allowlist-group")).toHaveValue("*");
  });

  it("sends both boxes and re-seeds them from what the server stored", async () => {
    installationsRef.current = {
      configured: true,
      installations: [
        {
          id: "i1",
          agent_id: "agent-1",
          status: "active",
          installed_at: "",
          allow_from_mode: "allowlist",
          allow_from: ["alice"],
          group_allow_from_mode: "allowlist",
          group_allow_from: ["778899"],
        },
      ],
    };
    // The echo collapses "alice,*" to the wildcard, which is what the gate now
    // is; the dialog must show that, not what was typed.
    const stored = {
      id: "i1",
      agent_id: "agent-1",
      status: "active" as const,
      installed_at: "",
      allow_from_mode: "unrestricted",
      allow_from: [],
      group_allow_from_mode: "allowlist",
      group_allow_from: ["778899"],
    };
    mutationsRef.allowlist.mutateAsync.mockImplementation(async () => {
      // Saving invalidates the list query, so the row comes back holding what the
      // server stored. The mocked query serves that as its data.
      installationsRef.current = { configured: true, installations: [stored] };
      return stored;
    });
    const view = renderUI(<TuituiTab />);
    await userEvent.click(screen.getByTestId("tuitui-installation-allowlist-edit"));
    const direct = await screen.findByTestId("tuitui-allowlist-direct");
    await userEvent.type(direct, ",*");
    await userEvent.click(screen.getByTestId("tuitui-allowlist-submit"));
    await waitFor(() =>
      expect(mutationsRef.allowlist.mutateAsync).toHaveBeenCalledWith({
        installationId: "i1",
        allow_from: "alice,*",
        group_allow_from: "778899",
      }),
    );
    await waitFor(() => expect(toastRef.success).toHaveBeenCalledWith("Allowlist updated"));
    expect(screen.queryByTestId("tuitui-allowlist-dialog")).toBeNull();
    // That save invalidated the list query, i.e. it refetches; the mocked query
    // only hands the new row over on the next render, so re-render here instead of
    // assuming the subscription exists.
    view.rerender(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <TuituiTab />
      </I18nProvider>,
    );
    // The row summary follows the stored gate: the personal side is open again.
    expect(screen.getByTestId("tuitui-allowlist-direct-open")).toHaveTextContent(
      "Anyone can DM it",
    );
    expect(screen.getByTestId("tuitui-allowlist-group-listed")).toHaveTextContent(
      "1 group allowed",
    );
    // Reopening shows the stored truth rather than what was typed.
    await userEvent.click(screen.getByTestId("tuitui-installation-allowlist-edit"));
    await waitFor(() =>
      expect(screen.getByTestId("tuitui-allowlist-direct")).toHaveValue("*"),
    );
    expect(screen.getByTestId("tuitui-allowlist-group")).toHaveValue("778899");
  });

  it("appends a refused id to the box its kind belongs to, without duplicating it", async () => {
    rejectionsRef.current = {
      rejections: [
        { id: "bob", kind: "user", count: 2, last_seen_at: "2026-01-06T10:00:00Z" },
        { id: "teams_t1_c2", kind: "group", count: 1, last_seen_at: "2026-01-05T10:00:00Z" },
      ],
    };
    installationsRef.current = {
      configured: true,
      installations: [
        {
          id: "i1",
          agent_id: "agent-1",
          status: "active",
          installed_at: "",
          allow_from_mode: "allowlist",
          allow_from: ["alice"],
          group_allow_from_mode: "allowlist",
          group_allow_from: [],
        },
      ],
    };
    renderUI(<TuituiTab />);
    await userEvent.click(screen.getByTestId("tuitui-installation-allowlist-edit"));
    // The refused ids are the only way to learn what to type here: group and
    // account ids are opaque platform strings nobody can read off a chat window.
    const refused = await screen.findByTestId("tuitui-allowlist-refusals");
    expect(refused).toHaveTextContent("bob");
    expect(refused).toHaveTextContent("teams_t1_c2");
    await userEvent.click(screen.getByTestId("tuitui-allowlist-refusal-bob"));
    await userEvent.click(screen.getByTestId("tuitui-allowlist-refusal-teams_t1_c2"));
    const direct = screen.getByTestId("tuitui-allowlist-direct");
    const group = screen.getByTestId("tuitui-allowlist-group");
    expect(direct).toHaveValue("alice, bob");
    // kind decides the box: the group key belongs in the group list, where it
    // matches, not the account list, where it would sit there refusing nobody.
    expect(group).toHaveValue("teams_t1_c2");
    // The empty group box no longer contradicts itself.
    expect(screen.queryByTestId("tuitui-allowlist-group-empty")).toBeNull();
    await userEvent.click(screen.getByTestId("tuitui-allowlist-refusal-bob"));
    expect(direct).toHaveValue("alice, bob");
  });

  it("keeps an open wildcard box alone when a refused id is clicked", async () => {
    rejectionsRef.current = {
      rejections: [{ id: "bob", kind: "user", count: 3, last_seen_at: "2026-01-06T10:00:00Z" }],
    };
    installationsRef.current = {
      configured: true,
      installations: [{ id: "i1", agent_id: "agent-1", status: "active", installed_at: "" }],
    };
    renderUI(<TuituiTab />);
    await userEvent.click(screen.getByTestId("tuitui-installation-allowlist-edit"));
    const direct = await screen.findByTestId("tuitui-allowlist-direct");
    await userEvent.click(screen.getByTestId("tuitui-allowlist-refusal-bob"));
    // `*` already admits bob, so adding his id would turn an open gate into a
    // closed one by accident.
    expect(direct).toHaveValue("*");
  });

  it("pages through refused subjects without dropping the draft", async () => {
    rejectionsRef.current = {
      rejections: [{ id: "bob", kind: "user", count: 1, last_seen_at: "2026-01-06T10:00:00Z" }],
      next_offset: 20,
    };
    rejectionsRef.next = {
      rejections: [{ id: "carol", kind: "group", count: 4, last_seen_at: "2026-01-01T10:00:00Z" }],
    };
    installationsRef.current = {
      configured: true,
      installations: [{ id: "i1", agent_id: "agent-1", status: "active", installed_at: "" }],
    };
    const view = renderUI(<TuituiTab />);
    await userEvent.click(screen.getByTestId("tuitui-installation-allowlist-edit"));
    const direct = await screen.findByTestId("tuitui-allowlist-direct");
    await userEvent.type(direct, ", typed-while-paging");
    await userEvent.click(screen.getByTestId("tuitui-allowlist-refusals-more"));
    // The next page is fetched under the continued offset...
    const carol = await screen.findByTestId("tuitui-allowlist-refusal-carol");
    expect(screen.queryByTestId("tuitui-allowlist-refusal-bob")).toBeNull();
    // ...and the ids already typed are still there: paging through the refusals
    // is not a reason to lose a half-written list.
    expect(direct).toHaveValue("*, typed-while-paging");
    // A short page is the only answer that can say there is nothing more, so the
    // affordance disappears with it.
    expect(screen.queryByTestId("tuitui-allowlist-refusals-more")).toBeNull();
    // Page two's id is a group, so it belongs to the group box -- which on this
    // bot is still `*`, where adding an id would only narrow an open gate.
    await userEvent.click(carol);
    expect(screen.getByTestId("tuitui-allowlist-group")).toHaveValue("*");
    // Reopening starts over at the first page: the last page browsed is not a
    // fact about the gate, and hiding the freshest refusals behind it would make
    // the list read as "nothing was turned away".
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    view.rerender(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <TuituiTab />
      </I18nProvider>,
    );
    await userEvent.click(screen.getByTestId("tuitui-installation-allowlist-edit"));
    await waitFor(() =>
      expect(screen.getByTestId("tuitui-allowlist-refusal-bob")).toBeInTheDocument(),
    );
    expect(direct).toHaveValue("*, typed-while-paging");
  });

  it("keeps the dialog open with the server's reason when the save is refused", async () => {
    mutationsRef.allowlist.mutateAsync.mockRejectedValue(
      new Error("group_allow_from: entry is longer than 128 characters"),
    );
    installationsRef.current = {
      configured: true,
      installations: [
        {
          id: "i1",
          agent_id: "agent-1",
          status: "active",
          installed_at: "",
          allow_from_mode: "allowlist",
          allow_from: ["alice"],
          group_allow_from_mode: "allowlist",
          group_allow_from: ["778899"],
        },
      ],
    };
    renderUI(<TuituiTab />);
    await userEvent.click(screen.getByTestId("tuitui-installation-allowlist-edit"));
    const group = await screen.findByTestId("tuitui-allowlist-group");
    await userEvent.type(group, ",oops");
    await userEvent.click(screen.getByTestId("tuitui-allowlist-submit"));
    // The reason names the field, so it is readable as "the second box", not as a
    // dead end.
    await waitFor(() =>
      expect(screen.getByTestId("tuitui-allowlist-error")).toHaveTextContent(
        "group_allow_from: entry is longer than 128 characters",
      ),
    );
    expect(toastRef.error).toHaveBeenCalled();
    expect(toastRef.success).not.toHaveBeenCalled();
    // Nothing was thrown away: the draft is still there to fix.
    expect(group).toHaveValue("778899,oops");
  });

  it("warns beside the box that an emptied list blocks everyone", async () => {
    installationsRef.current = {
      configured: true,
      installations: [
        {
          id: "i1",
          agent_id: "agent-1",
          status: "active",
          installed_at: "",
          allow_from_mode: "allowlist",
          allow_from: ["alice"],
          group_allow_from_mode: "allowlist",
          group_allow_from: ["778899"],
        },
      ],
    };
    renderUI(<TuituiTab />);
    await userEvent.click(screen.getByTestId("tuitui-installation-allowlist-edit"));
    const direct = await screen.findByTestId("tuitui-allowlist-direct");
    expect(screen.queryByTestId("tuitui-allowlist-direct-empty")).toBeNull();
    await userEvent.clear(direct);
    // The warning appears as the box is emptied, before the save that would make
    // it true: a cleared list is a working "block everyone", and an empty input
    // cannot show that on its own.
    expect(screen.getByTestId("tuitui-allowlist-direct-empty")).toHaveTextContent(
      "refusing every direct message",
    );
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
