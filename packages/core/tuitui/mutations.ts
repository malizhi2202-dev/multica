import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { tuituiKeys } from "./queries";
import type { RegisterTuituiBYORequest } from "../types";

// Write operations for the Tuitui channel. TanStack Query owns server data on
// web/desktop, so views never call `api.*` for these flows directly (AGENTS.md
// → State Rules): each mutation runs the request and invalidates exactly the
// workspace-scoped caches the response makes stale.

/** BYO app install: `{ app_id, app_secret }` for one agent, keyed into the
 * workspace by `wsId` (the caller renders it under that workspace's route). */
export function useRegisterTuituiBYO(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ agentId, ...body }: { agentId: string } & RegisterTuituiBYORequest) =>
      api.registerTuituiBYO(wsId, agentId, body),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: tuituiKeys.installations(wsId) });
    },
  });
}

/** Revoke (disconnect) one installation. The server soft-revokes the row; the
 * list refetch is the source of truth for the new status, so no optimistic
 * removal. */
export function useDeleteTuituiInstallation(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (installationId: string) =>
      api.deleteTuituiInstallation(wsId, installationId),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: tuituiKeys.installations(wsId) });
      await qc.invalidateQueries({ queryKey: tuituiKeys.groups(wsId) });
    },
  });
}

/** Forget one group observation from the workspace inventory. */
export function useForgetTuituiGroup(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ installationId, conversationId }: {
      installationId: string;
      conversationId: string;
    }) => api.forgetTuituiGroup(wsId, installationId, conversationId),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: tuituiKeys.groups(wsId) });
    },
  });
}

/** Redeem a binding token from /tuitui/bind. Account-level and one-shot: the
 * result only feeds the bind page's own state machine, so no cache is
 * invalidated — a later installations refetch observes the new binding. */
export function useRedeemTuituiBindingToken() {
  return useMutation({
    mutationFn: (token: string) => api.redeemTuituiBindingToken(token),
  });
}
