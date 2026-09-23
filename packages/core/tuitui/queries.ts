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
  // One page of the refused-subject list, keyed by the bot whose dialog asked
  // for it (`""` = every bot in this workspace) and by the page offset, so
  // "show more" adds a cache instead of replacing the first page under one key.
  rejections: (wsId: string, installationId = "", offset = 0) =>
    [...tuituiKeys.all(wsId), "rejections", installationId, offset] as const,
  // Prefix for every rejections page of this workspace, so one save invalidates
  // all of them without enumerating which dialog is open.
  rejectionsAll: (wsId: string) => [...tuituiKeys.all(wsId), "rejections"] as const,
};

/** TUITUI_REJECTIONS_PAGE_SIZE is one page of refused subjects. The dialog shows
 * a first screenful and offers more; the server clamps a larger ask. */
export const TUITUI_REJECTIONS_PAGE_SIZE = 20;

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

/** tuituiRejectionsOptions reads back the subjects one bot refused because no
 * allowlist named them (empty `installationId` = every bot in this workspace).
 * Nothing polls it: a refusal is only written while a bot is connected, and the
 * list is opened on purpose inside the allowlist dialog, so a fresh read per
 * open is what makes "someone tried this bot a minute ago" visible. */
export const tuituiRejectionsOptions = (wsId: string, installationId = "", offset = 0) =>
  queryOptions({
    queryKey: tuituiKeys.rejections(wsId, installationId, offset),
    queryFn: () =>
      api.listTuituiRejections(wsId, {
        installationId: installationId || undefined,
        offset: offset || undefined,
        limit: TUITUI_REJECTIONS_PAGE_SIZE,
      }),
    enabled: !!wsId,
  });
