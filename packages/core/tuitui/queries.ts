import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

/** Query key namespace for everything Tuitui-installation-related.
 * Workspace-scoped keys carry `wsId` per AGENTS.md. Realtime sync should
 * invalidate `installations(wsId)` on installation events so the Settings
 * panel updates without a manual refetch (e.g. after a binding lands the
 * install in another tab). */
export const tuituiKeys = {
  all: (wsId: string) => ["tuitui", wsId] as const,
  installations: (wsId: string) => [...tuituiKeys.all(wsId), "installations"] as const,
  groups: (wsId: string) => [...tuituiKeys.all(wsId), "groups"] as const,
};

export const tuituiInstallationsOptions = (wsId: string) =>
  queryOptions({
    queryKey: tuituiKeys.installations(wsId),
    queryFn: () => api.listTuituiInstallations(wsId),
    enabled: !!wsId,
  });

export const tuituiGroupsOptions = (wsId: string) =>
  queryOptions({
    queryKey: tuituiKeys.groups(wsId),
    queryFn: () => api.listTuituiGroups(wsId),
    enabled: !!wsId,
    // Group discovery arrives through Tuitui inbound WebSocket callbacks
    // rather than an HTTP mutation, so refresh lightly while the
    // permission-filtered Settings inventory is open. Stop after an error
    // instead of hammering a backend.
    refetchInterval: (query) =>
      query.state.status === "success" &&
      query.state.data?.group_discovery_supported === true
        ? 5_000
        : false,
  });
