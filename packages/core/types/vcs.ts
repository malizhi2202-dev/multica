/**
 * Token-based Git provider integration types (Forgejo, Gitea, GitLab). Unlike
 * GitHub there is no App/installation model: each workspace stores a
 * token-based connection to a provider instance. Pull requests mirrored from any
 * of these providers surface through the shared GitHubPullRequest shape, tagged
 * with the matching `provider`.
 */

export type VCSProvider = "forgejo" | "gitea" | "gitlab";

export interface VCSConnection {
  id: string;
  workspace_id: string;
  provider: VCSProvider;
  /** Instance base URL, e.g. https://forgejo.example.com (no trailing slash). */
  instance_url: string;
  /** Login (user or org) the stored access token authenticates as. */
  account_login: string;
  /** Absolute webhook endpoint to register on the provider. Empty when the server
   * has no public URL configured; the UI then prefixes `webhook_path`. */
  webhook_url: string;
  webhook_path: string;
  created_at: string;
}

export interface ListVCSConnectionsResponse {
  connections: VCSConnection[];
  /** Whether this deployment offers the integration at all (self-host only;
   * off on the managed cloud). When false the section is hidden entirely.
   * Older backends omit it; treat as true so the existing self-host UI still
   * renders (visibility is also gated by vcs_integration_available on
   * /api/config, which is the authoritative deployment signal). */
  available?: boolean;
  /** Whether the deployment can currently serve Git provider connections —
   * its signing secret is available. This is a real feature switch on the VCS
   * side (`available` / `vcs_integration_available` gate whether the section
   * is offered at all), distinct from the messaging channels' shared data key;
   * false disables the connect form. It is not something the operator pastes a
   * value for in the UI. Older backends omit it; treat as false (fail closed). */
  configured?: boolean;
  /** Whether the caller can connect / disconnect. Non-admins get false. */
  can_manage?: boolean;
}

export interface ConnectVCSRequest {
  provider: VCSProvider;
  instance_url: string;
  access_token: string;
}

export interface ConnectVCSResponse extends VCSConnection {
  /** One-time plaintext webhook secret to paste into the provider (HMAC secret
   * for Forgejo/Gitea, X-Gitlab-Token value for GitLab). Not retrievable
   * afterwards (stored encrypted); reconnecting rotates it. */
  webhook_secret: string;
}
