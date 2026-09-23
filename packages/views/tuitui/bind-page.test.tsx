// @vitest-environment jsdom

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { cleanup, render, screen, waitFor, fireEvent } from "@testing-library/react";
import { StrictMode, type ReactNode } from "react";
import { I18nProvider } from "@multica/core/i18n/react";
import { ApiError } from "@multica/core/api";
import enCommon from "../locales/en/common.json";

const TEST_RESOURCES = { en: { common: enCommon } };

const mockAuthState = vi.hoisted(() => ({
  user: null as { id: string; email: string } | null,
  isLoading: false,
}));
const mockNavigatePush = vi.hoisted(() => vi.fn());
// The page redeems through the core mutation, so the test swaps the hook for a
// stable mutateAsync stub instead of mounting a QueryClientProvider.
const mockRedeem = vi.hoisted(() => ({
  mutateAsync: vi.fn(),
  isPending: false,
}));

vi.mock("@multica/core/auth", () => {
  const useAuthStore = Object.assign(
    (selector?: (state: typeof mockAuthState) => unknown) =>
      selector ? selector(mockAuthState) : mockAuthState,
    { getState: () => mockAuthState },
  );
  return { useAuthStore };
});

vi.mock("../navigation/context", () => ({
  useNavigation: () => ({ push: mockNavigatePush }),
  useOptionalNavigation: () => ({ push: mockNavigatePush }),
}));

vi.mock("@multica/core/tuitui", () => ({
  useRedeemTuituiBindingToken: () => mockRedeem,
}));

import { TuituiBindPage } from "./bind-page";

afterEach(cleanup);

function I18nWrapper({ children }: { children: ReactNode }) {
  return (
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      {children}
    </I18nProvider>
  );
}

function renderPage(token: string | null) {
  return render(<TuituiBindPage token={token} />, { wrapper: I18nWrapper });
}

describe("TuituiBindPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // mockClear leaves queued once-implementations behind, which would let a
    // previous test's rejection leak into the next redeem call.
    mockRedeem.mutateAsync.mockReset();
    mockAuthState.user = null;
    mockAuthState.isLoading = false;
    mockRedeem.isPending = false;
  });

  it("shows the redeeming state while authentication is loading", () => {
    mockAuthState.isLoading = true;
    renderPage("tok123");
    expect(screen.getByText(/redeeming|linking/i)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /sign in/i })).toBeNull();
    expect(mockRedeem.mutateAsync).not.toHaveBeenCalled();
  });

  it("requires sign-in before redeeming", () => {
    renderPage("tok123");
    expect(screen.getByRole("button", { name: /sign in/i })).toBeInTheDocument();
    expect(mockRedeem.mutateAsync).not.toHaveBeenCalled();
  });

  it("preserves the token in the login next parameter", () => {
    renderPage("token with+/reserved");
    fireEvent.click(screen.getByRole("button", { name: /sign in/i }));

    expect(mockNavigatePush).toHaveBeenCalledTimes(1);
    const destination = mockNavigatePush.mock.calls[0]?.[0] as string;
    expect(destination).toContain("/login?next=");
    expect(decodeURIComponent(destination.split("next=")[1] ?? "")).toBe(
      "/tuitui/bind?token=token%20with%2B%2Freserved",
    );
  });

  it("redeems immediately when signed in and shows success", async () => {
    mockAuthState.user = { id: "u1", email: "u@example.com" };
    mockRedeem.mutateAsync.mockResolvedValue({
      workspace_id: "ws1",
      installation_id: "inst1",
      tuitui_user_id: "tt1",
    });

    renderPage("tok123");

    await waitFor(() => expect(mockRedeem.mutateAsync).toHaveBeenCalledWith("tok123"));
    await waitFor(() =>
        expect(screen.getByText(/you're linked/i)).toBeInTheDocument(),
      );
  });

  it("redeems a one-time token exactly once under StrictMode", async () => {
    mockAuthState.user = { id: "u1", email: "u@example.com" };
    mockRedeem.mutateAsync
      .mockResolvedValueOnce({
        workspace_id: "ws1",
        installation_id: "inst1",
        tuitui_user_id: "tt1",
      })
      .mockRejectedValueOnce(new ApiError("gone", 410, "Gone"));

    render(
      <StrictMode>
        <TuituiBindPage token="single-use-token" />
      </StrictMode>,
      { wrapper: I18nWrapper },
    );

    await waitFor(() =>
        expect(screen.getByText(/you're linked/i)).toBeInTheDocument(),
      );
    expect(mockRedeem.mutateAsync).toHaveBeenCalledTimes(1);
  });

  it("rejects a malformed success response", async () => {
    mockAuthState.user = { id: "u1", email: "u@example.com" };
    mockRedeem.mutateAsync.mockResolvedValue({
      workspace_id: "",
      installation_id: "",
      tuitui_user_id: "",
    });

    renderPage("tok123");

    await waitFor(() =>
      expect(screen.getByText(/couldn.t complete|error/i)).toBeInTheDocument(),
    );
    expect(screen.getByText(/try again|administrator/i)).toBeInTheDocument();
  });

  it("shows the missing-token error without calling the API", () => {
    renderPage(null);
    expect(screen.getByText(/missing its token|no token/i)).toBeInTheDocument();
    expect(mockRedeem.mutateAsync).not.toHaveBeenCalled();
  });

  // Failure copy is classified by HTTP status, not server wording — so the
  // same status always maps to the same recovery hint.
  it.each([
    [410, /invalid or expired/i],
    [409, /already (been )?linked|already bound/i],
    [403, /member/i],
    [500, /went wrong|unexpected/i],
  ])("maps a %i redemption failure to the right message", async (status, expected) => {
    mockAuthState.user = { id: "u1", email: "u@example.com" };
    mockRedeem.mutateAsync.mockRejectedValue(
      new ApiError("server said something", status, "Error"),
    );

    renderPage("tok123");

    await waitFor(() => expect(screen.getByText(expected)).toBeInTheDocument());
  });

  it("falls back to the unknown error for non-ApiError rejections", async () => {
    mockAuthState.user = { id: "u1", email: "u@example.com" };
    mockRedeem.mutateAsync.mockRejectedValue(new Error("network exploded"));

    renderPage("tok123");

    await waitFor(() =>
      expect(screen.getByText(/went wrong|unexpected/i)).toBeInTheDocument(),
    );
  });
});
