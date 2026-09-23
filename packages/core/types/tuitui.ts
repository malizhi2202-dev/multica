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
}

/** Post-redemption echo: the Tuitui user id the token carried is now bound to
 * the logged-in Multica user in this workspace/installation. */
export interface RedeemTuituiBindingTokenResponse {
  workspace_id: string;
  installation_id: string;
  tuitui_user_id: string;
}
