"use client";

import { useEffect, useRef, useState } from "react";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import { Button } from "@multica/ui/components/ui/button";
import { ApiError } from "@multica/core/api";
import { useAuthStore } from "@multica/core/auth";
import { useRedeemTuituiBindingToken } from "@multica/core/tuitui";
import { useNavigation } from "../navigation";
import { useT } from "../i18n";

type RedeemState =
  | { kind: "idle" }
  | { kind: "redeeming" }
  | { kind: "done"; workspaceId: string; installationId: string }
  | { kind: "needs-auth" }
  | { kind: "error"; reason: string };

// TuituiBindPage is the destination the bot's "link your account" prompt
// points at, mirroring DingTalkBindPage. Auth is required before redeeming:
// the redeemer's Multica identity comes from the session, never the token.
// The redemption itself runs through the core mutation so TanStack Query owns
// the write.
export function TuituiBindPage({ token }: { token: string | null }) {
  const { t } = useT("common");
  const user = useAuthStore((s) => s.user);
  const isAuthLoading = useAuthStore((s) => s.isLoading);
  const navigation = useNavigation();
  const redeemBindingToken = useRedeemTuituiBindingToken();
  const [state, setState] = useState<RedeemState>({ kind: "idle" });
  const redeemingToken = useRef<string | null>(null);

  useEffect(() => {
    if (!token) {
      setState({ kind: "error", reason: "missing_token" });
      return;
    }
    if (isAuthLoading) return;
    if (!user) {
      setState({ kind: "needs-auth" });
      return;
    }
    if (state.kind !== "idle" && state.kind !== "needs-auth") return;
    if (redeemingToken.current === token) return;
    redeemingToken.current = token;
    setState({ kind: "redeeming" });
    (async () => {
      try {
        const resp = await redeemBindingToken.mutateAsync(token);
        if (!resp.workspace_id || !resp.installation_id || !resp.tuitui_user_id) {
          throw new Error("Tuitui binding returned a malformed response");
        }
        setState({
          kind: "done",
          workspaceId: resp.workspace_id,
          installationId: resp.installation_id,
        });
      } catch (e) {
        setState({ kind: "error", reason: redemptionFailureReason(e) });
      }
    })();
  }, [token, user, isAuthLoading, state.kind, redeemBindingToken]);

  return (
    <div className="mx-auto flex min-h-screen max-w-md flex-col items-center justify-center p-6">
      <Card className="w-full">
        <CardContent className="space-y-4">
          <h1 className="text-title font-semibold">{t(($) => $.tuitui_bind.page_title)}</h1>
          {state.kind === "idle" || state.kind === "redeeming" ? (
            <p className="text-body text-muted-foreground">{t(($) => $.tuitui_bind.redeeming)}</p>
          ) : state.kind === "needs-auth" ? (
            <>
              <p className="text-body text-muted-foreground">
                {t(($) => $.tuitui_bind.needs_auth_description)}
              </p>
              <Button
                size="sm"
                onClick={() =>
                  navigation.push(
                    `/login?next=${encodeURIComponent(
                      `/tuitui/bind?token=${encodeURIComponent(token ?? "")}`,
                    )}`,
                  )
                }
              >
                {t(($) => $.tuitui_bind.sign_in)}
              </Button>
            </>
          ) : state.kind === "done" ? (
            <>
              <p className="text-body font-medium">{t(($) => $.tuitui_bind.done_title)}</p>
              <p className="text-caption text-muted-foreground">
                {t(($) => $.tuitui_bind.done_description)}
              </p>
            </>
          ) : (
            <>
              <p className="text-body font-medium">{t(($) => $.tuitui_bind.error_title)}</p>
              <p className="text-caption text-muted-foreground">
                {(() => {
                  switch (state.reason) {
                    case "missing_token":
                      return t(($) => $.tuitui_bind.error_missing_token);
                    case "expired":
                      return t(($) => $.tuitui_bind.error_expired);
                    case "already_bound":
                      return t(($) => $.tuitui_bind.error_already_bound);
                    case "not_member":
                      return t(($) => $.tuitui_bind.error_not_member);
                    default:
                      return t(($) => $.tuitui_bind.error_unknown);
                  }
                })()}
              </p>
              <p className="text-micro text-muted-foreground">
                {t(($) => $.tuitui_bind.error_admin_hint)}
              </p>
            </>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

// Classify a redeem failure by the server's HTTP status, not the error text:
// the backend returns 410 (invalid/expired), 409 (already bound to another
// user) or 403 (not a workspace member). Matching ApiError.status keeps the
// branch copy decoupled from the exact server wording.
function redemptionFailureReason(err: unknown): string {
  if (!(err instanceof ApiError)) return "unknown";
  switch (err.status) {
    case 410:
      return "expired";
    case 409:
      return "already_bound";
    case 403:
      return "not_member";
    default:
      return "unknown";
  }
}
