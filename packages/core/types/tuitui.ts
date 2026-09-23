/** A Tuitui (推推) robot installation bound to a single Multica agent.
 *
 * Wire shape mirrors the DingTalk installation contract: Tuitui is the fifth
 * isomorphic IM channel (bring-your-own-app install, per-agent
 * channel_installation row, group discovery, user-level binding redeem). New
 * fields the backend adds in the future MUST default to optional so older
 * desktop builds keep parsing the response — see AGENTS.md → API
 * Compatibility. */
export interface TuituiInstallation {
  id: string;
  workspace_id: string;
  agent_id: string;
  installer_user_id: string;
  /** Soft-revoked rows keep status "revoked": never assume "unknown" means
   * "active". */
  status: "active" | "revoked" | string;
  installed_at: string;
  created_at: string;
  updated_at: string;
  /** False only when a workspace admin is viewing an orphaned installation
   * whose Agent no longer exists. Optional for older backends. */
  agent_available?: boolean;
  /** Effective Tuitui server host this bot dials — the normalized host the
   * BYO dialog's single "server address" field resolved to (an IPv6 literal
   * keeps its brackets). Echoed so the user can confirm which server they
   * connected. Optional for older backends, which never sent it. */
  host?: string;
  /** Effective port behind `host`, with the adapter's default already applied
   * server-side. Optional for older backends. */
  port?: number;
  /** Tuitui user ids linked by the currently authenticated Multica user for
   * this bot. Member-scoped so the member-visible installation endpoint does
   * not disclose other members' Tuitui identities. */
  bound_tuitui_user_ids?: string[];
  /** Which of the two inbound gates this bot applies. Optional because a
   * backend predating the allowlist sends neither field, and the panel then
   * treats both gates as unrestricted (which is exactly how such a bot
   * behaves). `unrestricted` answers everyone — the operator stored `*`, or
   * never configured the gate; `allowlist` answers only the ids in the
   * matching list, and a `allowlist` with an EMPTY list is a deliberate
   * "nobody", never a synonym of `unrestricted`. */
  allow_from_mode?: TuituiAllowlistMode;
  /** The Tuitui accounts allowed to DM this bot. Empty when
   * `allow_from_mode` is `unrestricted`, so an open bot can never be misread
   * as a closed one. */
  allow_from?: string[];
  /** Gate for group chats and teams channels, applied first; a conversation
   * missing from it falls back to `allow_from` for its sender. */
  group_allow_from_mode?: TuituiAllowlistMode;
  /** The group ids / teams channel keys allowed, as above. */
  group_allow_from?: string[];
}

/** Echo of one inbound allowlist gate. Unknown values degrade to the string
 * type rather than throwing: an older client shows the ids it was given and
 * leaves the label generic. */
export type TuituiAllowlistMode = "unrestricted" | "allowlist" | string;

/** One subject (a person or a conversation) a Tuitui bot turned away because
 * no allowlist named it. The panel offers these as one-click entries, which is
 * the only practical way to fill the lists in: Tuitui ids are opaque numbers
 * nobody can type from memory. */
export interface TuituiRejection {
  /** A Tuitui user account when `kind` is "user", otherwise a group id or a
   * channel-scoped `teams_{team}_{channel}` key — pasteable as-is. */
  id: string;
  /** Which of the two lists this id belongs in. */
  kind: TuituiRejectionKind;
  /** The bot that refused, and the agent behind it. */
  installation_id: string;
  agent_id: string;
  /** How many messages were turned away for this subject, and when the newest
   * one arrived. Both only grow while the bot is connected, which is what
   * distinguishes "add this id" from "guess an id". */
  count: number;
  last_seen_at: string;
}

export type TuituiRejectionKind = "user" | "group" | string;

export interface ListTuituiRejectionsResponse {
  rejections: TuituiRejection[];
  /** Offset for the next page, absent when the list is complete. */
  next_offset?: number;
}

export interface ListTuituiRejectionsParams {
  /** Restrict the list to one connected bot — what an installation's own dialog
   * shows, so a workspace with several bots never mixes their refusals into one
   * suggestion list. */
  installationId?: string;
  offset?: number;
  limit?: number;
}

export interface ListTuituiInstallationsResponse {
  installations: TuituiInstallation[];
  /** Whether the Tuitui integration is currently usable on this deployment.
   * App credentials are entered in the UI and sealed at rest with the
   * deployment's data key (an environment override is optional), so false
   * means "this integration is currently unavailable" — not "an operator has
   * no env var to set". When false (or absent — the schema fails closed) the
   * panel renders the "currently unavailable" state instead of the install
   * entry points. */
  configured: boolean;
}

/** One connected Multica bot observed in a Tuitui group conversation. Teams
 * (channel) conversations have no platform chat id, so the backend composes
 * "teams_{team}_{channel}[_{thread}]" and maps it onto the group chat type. */
export interface TuituiGroupBot {
  installation_id: string;
  agent_id: string;
  /** Readable Tuitui bot name. Empty when the lookup has not run yet or the
   * app lacks the permission to read it. */
  bot_name: string;
  /** Machine-readable reason the readable identity is unavailable. */
  bot_identity_issue: string;
  /** Latest accepted @mention from this group, in RFC 3339. Optional for
   * compatibility with older backends; empty when no durable message remains. */
  last_active_at?: string;
  /** Number of deduplicated group messages accepted for this bot. */
  mention_count?: number;
}

/** A Tuitui group observed after a validated @bot callback. */
export interface TuituiGroup {
  conversation_id: string;
  conversation_title: string;
  bots: TuituiGroupBot[];
}

export interface ListTuituiGroupsResponse {
  groups: TuituiGroup[];
  /** False when an installed client is connected to a backend that predates
   * group discovery. Callers use it to avoid polling an absent endpoint. */
  group_discovery_supported: boolean;
  /** Historical observations outside the active window, keyed by bot
   * installation. Additive for compatibility with older servers. */
  inactive_group_counts?: Record<string, number>;
  /** App-wide identity keyed by installation, available even when every group
   * for that bot is outside the active window. */
  bot_identities?: Record<string, TuituiGroupBot>;
  /** Offset for the next inactive page. Absent when the page is complete. */
  next_offset?: number;
}

export interface ListTuituiGroupsParams {
  activity?: "inactive";
  installationId?: string;
  offset?: number;
  limit?: number;
}

/** Request body for a bring-your-own-app (BYO) install: the address of the
 * Tuitui server Multica must connect to plus the app_id and app_secret of the
 * Tuitui robot application the operator created. The backend resolves
 * `base_url` into the stored host + port pair — a missing or unparsable
 * address is a 400, never a silent fallback to the public cloud default —
 * then validates both credentials before persisting, and returns the created
 * TuituiInstallation with the effective host/port echoed. */
export interface RegisterTuituiBYORequest {
  /** "https://host:port", "wss://host", or a bare "host[:port]". Required. */
  base_url: string;
  app_id: string;
  app_secret: string;
  // No allowlist fields here on purpose: this request replaces the whole
  // installation, so carrying the two lists through it would demand the
  // app_secret again for what is meant to be a one-box edit. A freshly connected
  // bot answers everyone until its lists are written.
}

/** Body of the narrow allowlist write (PUT
 * /api/workspaces/{id}/tuitui/installations/{installationId}/allowlists): the
 * comma-separated text of the two inbound lists, exactly as the panel's inputs
 * hold them. The backend trims, de-duplicates, validates, and stores them, and
 * answers with the echoed installation so the dialog shows the effective lists
 * rather than what was typed.
 *
 * Omitting a key means "leave that list exactly as it is", which is why both
 * fields are optional; sending `""` is not "nothing to say" but "clear it", and
 * stores an empty list — nobody may talk to this bot on that side. `*` allows
 * all, and `*` beside concrete ids is stored as the wildcard alone because the
 * gate tests for the wildcard first.
 *
 * The write deliberately does not go through the BYO install endpoint: that one
 * replaces the whole installation and requires the app_id + app_secret again,
 * which the panel never holds (the secret is sealed at rest and not echoed). */
export interface UpdateTuituiAllowlistsRequest {
  allow_from?: string;
  group_allow_from?: string;
}

/** Post-redemption echo: the Tuitui user id the token carried is now bound to
 * the logged-in Multica user in this workspace/installation. */
export interface RedeemTuituiBindingTokenResponse {
  workspace_id: string;
  installation_id: string;
  tuitui_user_id: string;
}
