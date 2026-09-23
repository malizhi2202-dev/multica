"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { Plus, Trash2 } from "lucide-react";
import { cn } from "@multica/ui/lib/utils";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from "@multica/ui/components/ui/tooltip";
import { useAuthStore } from "@multica/core/auth";
import { useConfigStore } from "@multica/core/config";
import { useWorkspaceId } from "@multica/core/hooks";
import {
  agentListOptions,
  memberListOptions,
} from "@multica/core/workspace/queries";
import { useActorName } from "@multica/core/workspace/hooks";
import {
  tuituiGroupsOptions,
  tuituiInstallationsOptions,
  useDeleteTuituiInstallation,
  useForgetTuituiGroup,
  useRegisterTuituiBYO,
} from "@multica/core/tuitui";
import type {
  TuituiGroup,
  TuituiGroupBot,
  TuituiInstallation,
} from "@multica/core/types";
import { ActorAvatar } from "../../common/actor-avatar";
import { useT, useTimeAgo } from "../../i18n";
import { TuituiMark } from "./tuitui-mark";

// formatInstalledAt renders the install timestamp defensively: the schema
// defaults installed_at to "" and the backend can emit a zero-value timestamp
// (0001-01-01T…) for a never-set time, either of which would otherwise surface
// as "Invalid Date" or a year-1 date. Fall back to a neutral placeholder.
function formatInstalledAt(value: string, locale: string): string {
  const t = Date.parse(value);
  if (!value || Number.isNaN(t) || t <= 0) return "—";
  return new Intl.DateTimeFormat(locale, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(new Date(t));
}

export function getTuituiBotIdentity(
  groups: TuituiGroup[],
  installationId: string,
): TuituiGroupBot | undefined {
  const bots = groups.flatMap((group) =>
    group.bots.filter((bot) => bot.installation_id === installationId),
  );
  const identity = bots.find((bot) => bot.bot_name) ?? bots[0];
  const issue = bots.find((bot) => bot.bot_identity_issue)?.bot_identity_issue;
  return identity
    ? { ...identity, bot_identity_issue: issue ?? identity.bot_identity_issue }
    : undefined;
}

// TuituiConnectionLabel is the green-dot "connected" row shared by the
// workspace panel and the agent side. bot_identity_issue stays as plain data
// behind the identity text: unlike DingTalk's single known permission gap, the
// Tuitui adapter reports machine-readable reasons the UI renders generically.
export function TuituiConnectionLabel({
  botName,
  showBotIdentity = true,
  className,
}: {
  botName?: string;
  showBotIdentity?: boolean;
  className?: string;
}) {
  const { t } = useT("settings");
  const botIdentityText =
    botName || t(($) => $.tuitui.bot_identity_unavailable);
  return (
    <span
      className={cn(
        "inline-flex min-w-0 items-center gap-2 text-caption text-muted-foreground",
        className,
      )}
    >
      <span
        className="inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-emerald-500"
        aria-hidden="true"
      />
      <span className="inline-flex min-w-0 items-center">
        <span className="shrink-0">
          {t(($) => $.tuitui.agent_bot_connected_label)}
        </span>
        {showBotIdentity && (
          <span className="ml-1 truncate text-foreground">{botIdentityText}</span>
        )}
      </span>
    </span>
  );
}

// TuituiBotGroups renders the group observations for one installation within
// the parent's active-window groups query.
export function TuituiBotGroups({
  groups,
  installationId,
  canForget = false,
  showDescription = true,
  className,
}: {
  groups: TuituiGroup[];
  installationId: string;
  canForget?: boolean;
  showDescription?: boolean;
  className?: string;
}) {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const timeAgo = useTimeAgo();
  const [forgetTarget, setForgetTarget] = useState<TuituiGroup | null>(null);
  const forgetGroup = useForgetTuituiGroup(wsId);

  const observed = groups
    .flatMap((group) => {
      const bot = group.bots.find(
        (candidate) => candidate.installation_id === installationId,
      );
      return bot ? [{ group, bot }] : [];
    })
    .sort((left, right) => {
      const leftActiveAt = Date.parse(left.bot.last_active_at ?? "");
      const rightActiveAt = Date.parse(right.bot.last_active_at ?? "");
      const leftIsActive = !Number.isNaN(leftActiveAt);
      const rightIsActive = !Number.isNaN(rightActiveAt);
      if (leftIsActive && rightIsActive && leftActiveAt !== rightActiveAt) {
        return rightActiveAt - leftActiveAt;
      }
      if (leftIsActive !== rightIsActive) return leftIsActive ? -1 : 1;
      const leftTitle = left.group.conversation_title;
      const rightTitle = right.group.conversation_title;
      if (leftTitle !== rightTitle) {
        if (!leftTitle) return 1;
        if (!rightTitle) return -1;
        return leftTitle < rightTitle ? -1 : 1;
      }
      return left.group.conversation_id.localeCompare(right.group.conversation_id);
    });

  async function handleForget() {
    if (!forgetTarget || forgetGroup.isPending) return;
    try {
      await forgetGroup.mutateAsync({
        installationId,
        conversationId: forgetTarget.conversation_id,
      });
      toast.success(t(($) => $.tuitui.group_forget_success));
      setForgetTarget(null);
    } catch (error) {
      toast.error(
        error instanceof Error
          ? error.message
          : t(($) => $.tuitui.group_forget_failed),
      );
    }
  }

  return (
    <div className={cn("space-y-2 pt-3", className)} data-testid="tuitui-bot-groups">
      <div className="flex items-center gap-2">
        <h4 className="text-body font-medium text-pretty">
          {t(($) => $.tuitui.groups_title)}
        </h4>
        <span className="rounded-full bg-muted px-2 py-0.5 text-micro tabular-nums text-muted-foreground">
          {t(($) => $.tuitui.groups_count, { count: observed.length })}
        </span>
      </div>
      {showDescription && (
        <p className="text-caption leading-relaxed text-muted-foreground">
          {t(($) => $.tuitui.groups_description)}
        </p>
      )}
      {observed.length === 0 ? (
        <p className="text-caption text-muted-foreground">
          {t(($) => $.tuitui.groups_empty)}
        </p>
      ) : (
        <div>
          {observed.map(({ group, bot }) => (
            <div
              key={group.conversation_id}
              className="group space-y-1.5 border-t py-2.5"
              data-testid="tuitui-group-item"
            >
              <div className="flex min-w-0 items-baseline justify-between gap-3">
                <div className="flex min-w-0 flex-1 items-baseline gap-3">
                  <p className="min-w-0 truncate text-caption font-medium">
                    {group.conversation_title || t(($) => $.tuitui.group_untitled)}
                  </p>
                  {canForget && (
                    <button
                      type="button"
                      className="pointer-events-none shrink-0 text-micro text-muted-foreground opacity-0 underline-offset-2 transition-opacity group-hover:pointer-events-auto group-hover:opacity-100 group-focus-within:pointer-events-auto group-focus-within:opacity-100 hover:text-foreground hover:underline focus-visible:pointer-events-auto focus-visible:opacity-100"
                      onClick={() => setForgetTarget(group)}
                    >
                      {t(($) => $.tuitui.group_forget)}
                    </button>
                  )}
                </div>
                <div className="flex min-w-0 max-w-[60%] items-center">
                  <TooltipProvider delay={0}>
                    <Tooltip>
                      <TooltipTrigger
                        render={
                          <code
                            tabIndex={0}
                            translate="no"
                            aria-label={`${t(($) => $.tuitui.conversation_id_label)} ${group.conversation_id}`}
                            className="block min-w-0 truncate font-mono text-micro text-faint-foreground transition-colors group-hover:text-muted-foreground focus-visible:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                          >
                            {group.conversation_id}
                          </code>
                        }
                      />
                      <TooltipContent side="top">
                        {t(($) => $.tuitui.conversation_id_label)}
                      </TooltipContent>
                    </Tooltip>
                  </TooltipProvider>
                </div>
              </div>
              {bot.last_active_at && (
                <p
                  className="text-micro tabular-nums text-muted-foreground"
                  data-testid="tuitui-group-activity"
                >
                  {t(($) => $.tuitui.group_last_active, {
                    time: timeAgo(bot.last_active_at),
                  })}
                  <span className="mx-1.5" aria-hidden="true">·</span>
                  {t(($) => $.tuitui.group_mentions, {
                    count: bot.mention_count ?? 0,
                  })}
                </p>
              )}
            </div>
          ))}
        </div>
      )}
      <AlertDialog
        open={!!forgetTarget}
        onOpenChange={(open) => {
          if (!open && !forgetGroup.isPending) setForgetTarget(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.tuitui.group_forget_title)}</AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.tuitui.group_forget_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={forgetGroup.isPending}>
              {t(($) => $.tuitui.group_forget_cancel)}
            </AlertDialogCancel>
            <AlertDialogAction onClick={handleForget} disabled={forgetGroup.isPending}>
              {t(($) => $.tuitui.group_forget_confirm)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

// TuituiByoFields is the connection form shared by the Settings add dialog and
// the agent-side Connect dialog: the address of the Tuitui server to dial plus
// the credential pair. The address is a plain visible input (it is a hostname,
// not a secret, and mis-typing it connects the bot to the wrong server); the
// credential inputs stay masked like DingTalk's — these values are secrets
// pasted out of the Tuitui app console. base_url arrives verbatim at the API;
// resolving it into host + port is the handler's single job, so no parsing or
// defaulting happens here.
function TuituiByoFields({
  baseUrl,
  appId,
  appSecret,
  onChange,
  disabled,
  error = "",
}: {
  baseUrl: string;
  appId: string;
  appSecret: string;
  onChange: (next: { baseUrl: string; appId: string; appSecret: string }) => void;
  disabled: boolean;
  /** Server rejection (a 400 naming the exact violation), shown verbatim. */
  error?: string;
}) {
  const { t } = useT("settings");
  return (
    <>
      <div className="space-y-1.5">
        <Label
          htmlFor="tuitui-byo-base-url"
          className="text-caption text-muted-foreground"
        >
          {t(($) => $.tuitui.byo_baseurl_label)}
        </Label>
        <Input
          id="tuitui-byo-base-url"
          data-testid="tuitui-byo-base-url"
          type="text"
          value={baseUrl}
          onChange={(e) => onChange({ baseUrl: e.target.value, appId, appSecret })}
          // Literal, not a locale key: the example is an address, identical
          // in every language. Scheme included on purpose — it is the
          // canonical spelling the hint describes.
          placeholder="https://tuitui.internal:8282"
          autoComplete="off"
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          disabled={disabled}
          aria-invalid={!!error}
          aria-describedby={error ? "tuitui-byo-base-url-error" : undefined}
        />
        <p className="text-caption leading-relaxed text-muted-foreground">
          {t(($) => $.tuitui.byo_baseurl_hint)}
        </p>
        {/* Inline error beside the address it concerns, rendered only when a
            submit was rejected (required-ness itself is enforced by the
            submit gate, never as a standalone error). The localized lead-in
            frames the backend's precise violation, which is shown verbatim
            because it names the exact reason to fix. */}
        {error ? (
          <p
            id="tuitui-byo-base-url-error"
            role="alert"
            data-testid="tuitui-byo-error"
            className="text-caption text-destructive"
          >
            {`${t(($) => $.tuitui.byo_baseurl_error)} ${error}`}
          </p>
        ) : null}
      </div>
      <div className="space-y-1.5">
        <Label
          htmlFor="tuitui-byo-app-id"
          className="text-caption text-muted-foreground"
        >
          {t(($) => $.tuitui.byo_appid_label)}
        </Label>
        <Input
          id="tuitui-byo-app-id"
          data-testid="tuitui-byo-app-id"
          type="password"
          value={appId}
          onChange={(e) => onChange({ baseUrl, appId: e.target.value, appSecret })}
          autoComplete="off"
          spellCheck={false}
          disabled={disabled}
        />
      </div>
      <div className="space-y-1.5">
        <Label
          htmlFor="tuitui-byo-app-secret"
          className="text-caption text-muted-foreground"
        >
          {t(($) => $.tuitui.byo_appsecret_label)}
        </Label>
        <Input
          id="tuitui-byo-app-secret"
          data-testid="tuitui-byo-app-secret"
          type="password"
          value={appSecret}
          onChange={(e) => onChange({ baseUrl, appId, appSecret: e.target.value })}
          autoComplete="off"
          spellCheck={false}
          disabled={disabled}
        />
      </div>
    </>
  );
}

// TuituiTab is the workspace settings panel for Tuitui robot installations.
// Unlike DingTalk — whose BYO dialog only exists per-agent — this panel also
// owns the "add installation" form: the install path stays per-agent (the
// (workspace_id, agent_id, channel_type) UNIQUE in channel_installation), so
// the form includes an Agent picker and submits the same
// POST /api/workspaces/{id}/tuitui/install/byo with ?agent_id=.
// Disconnect and the group inventory are workspace owner/admin-only, matching
// the backend gates.
export function TuituiTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const user = useAuthStore((s) => s.user);

  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const currentMember = members.find((m) => m.user_id === user?.id) ?? null;
  const canManage =
    currentMember?.role === "owner" || currentMember?.role === "admin";

  const { data, isLoading } = useQuery(tuituiInstallationsOptions(wsId));
  const installations = data?.installations ?? [];
  const configured = data?.configured === true;
  const { data: groupsData } = useQuery({
    ...tuituiGroupsOptions(wsId),
    enabled: configured && installations.some((inst) => inst.status === "active"),
  });
  const groups = groupsData?.groups ?? [];

  const deleteInstallation = useDeleteTuituiInstallation(wsId);
  const [disconnectTarget, setDisconnectTarget] = useState<string | null>(null);

  const [addOpen, setAddOpen] = useState(false);
  const [addAgentId, setAddAgentId] = useState("");
  const [addBaseUrl, setAddBaseUrl] = useState("");
  const [addAppId, setAddAppId] = useState("");
  const [addAppSecret, setAddAppSecret] = useState("");
  const [addError, setAddError] = useState("");
  const registerBYO = useRegisterTuituiBYO(wsId);
  const { data: agents = [] } = useQuery({
    ...agentListOptions(wsId),
    enabled: canManage && !!wsId,
  });

  function closeAddDialog() {
    if (registerBYO.isPending) return;
    setAddOpen(false);
    setAddAgentId("");
    setAddBaseUrl("");
    setAddAppId("");
    setAddAppSecret("");
    setAddError("");
  }

  async function handleAddSubmit() {
    const agentId = addAgentId;
    const base_url = addBaseUrl.trim();
    const app_id = addAppId.trim();
    const app_secret = addAppSecret.trim();
    // base_url is required like the credentials: an empty address must never
    // fall through to a server-side default target.
    if (registerBYO.isPending || !agentId || !base_url || !app_id || !app_secret) return;
    try {
      await registerBYO.mutateAsync({ agentId, base_url, app_id, app_secret });
      toast.success(t(($) => $.tuitui.byo_success_toast));
      setAddOpen(false);
      setAddAgentId("");
      setAddBaseUrl("");
      setAddAppId("");
      setAddAppSecret("");
      setAddError("");
    } catch (e) {
      const message =
        e instanceof Error ? e.message : t(($) => $.tuitui.byo_failed_toast);
      setAddError(message);
      toast.error(message);
    }
  }

  async function handleDisconnect() {
    if (!disconnectTarget || deleteInstallation.isPending) return;
    try {
      await deleteInstallation.mutateAsync(disconnectTarget);
      toast.success(t(($) => $.tuitui.toast_disconnected));
      setDisconnectTarget(null);
    } catch (e) {
      toast.error(
        e instanceof Error ? e.message : t(($) => $.tuitui.toast_disconnect_failed),
      );
    }
  }

  const addCanSubmit =
    !!addAgentId &&
    addBaseUrl.trim() !== "" &&
    addAppId.trim() !== "" &&
    addAppSecret.trim() !== "";

  return (
    <div className="space-y-8">
      {!configured ? (
        <Card>
          <CardContent className="space-y-2">
            <p className="text-body font-medium">{t(($) => $.tuitui.not_enabled_title)}</p>
            <p className="text-caption text-muted-foreground">
              {t(($) => $.tuitui.not_enabled_description)}
            </p>
          </CardContent>
        </Card>
      ) : (
        <section className="space-y-4">
          <div className="flex items-center justify-between gap-3">
            <h2 className="text-body font-semibold">
              {t(($) => $.tuitui.connections_title)}
            </h2>
            {canManage && (
              <Button
                variant="outline"
                size="sm"
                onClick={() => setAddOpen(true)}
                data-testid="tuitui-add-installation"
              >
                <Plus className="h-3 w-3" aria-hidden="true" />
                {t(($) => $.tuitui.add_installation)}
              </Button>
            )}
          </div>
          {isLoading ? (
            <Card>
              <CardContent>
                <p className="text-body text-muted-foreground">{t(($) => $.tuitui.loading)}</p>
              </CardContent>
            </Card>
          ) : installations.length === 0 ? (
            <Card>
              <CardContent className="space-y-2">
                <p className="text-body font-medium">{t(($) => $.tuitui.empty_title)}</p>
                <p className="text-caption text-muted-foreground">
                  {t(($) => $.tuitui.empty_description)}
                </p>
              </CardContent>
            </Card>
          ) : (
            <Card className="py-0">
              <CardContent className="divide-y divide-border/70">
                {installations.map((inst) => (
                  <TuituiInstallationRow
                    key={inst.id}
                    installation={inst}
                    canManage={canManage}
                    onDisconnect={() => setDisconnectTarget(inst.id)}
                    groups={groups}
                    botIdentity={inst.id ? groupsData?.bot_identities?.[inst.id] : undefined}
                  />
                ))}
              </CardContent>
            </Card>
          )}
        </section>
      )}

      <Dialog open={addOpen} onOpenChange={(v) => (v ? setAddOpen(true) : closeAddDialog())}>
        <DialogContent
          className="gap-0 overflow-hidden p-0 sm:max-w-lg"
          data-testid="tuitui-add-dialog"
        >
          <DialogHeader className="gap-1 border-b px-5 py-3">
            <DialogTitle className="text-title-sm font-semibold">
              {t(($) => $.tuitui.byo_dialog_title)}
            </DialogTitle>
            <p className="text-caption text-muted-foreground">
              {t(($) => $.tuitui.byo_dialog_description)}
            </p>
          </DialogHeader>

          <div className="space-y-4 p-5">
            <div className="space-y-1.5">
              <Label
                htmlFor="tuitui-byo-agent"
                className="text-caption text-muted-foreground"
              >
                {t(($) => $.tuitui.byo_agent_label)}
              </Label>
              <Select
                items={agents.map((agent) => ({ value: agent.id, label: agent.name }))}
                value={addAgentId || undefined}
                onValueChange={(value) => setAddAgentId(value ?? "")}
                disabled={registerBYO.isPending || agents.length === 0}
              >
                <SelectTrigger id="tuitui-byo-agent" data-testid="tuitui-byo-agent">
                  <SelectValue placeholder={t(($) => $.tuitui.byo_agent_placeholder)} />
                </SelectTrigger>
                <SelectContent>
                  {agents.map((agent) => (
                    <SelectItem key={agent.id} value={agent.id}>
                      {agent.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <TuituiByoFields
              baseUrl={addBaseUrl}
              appId={addAppId}
              appSecret={addAppSecret}
              error={addError}
              onChange={({ baseUrl, appId, appSecret }) => {
                setAddBaseUrl(baseUrl);
                setAddAppId(appId);
                setAddAppSecret(appSecret);
                // The error described the previous submit attempt.
                setAddError("");
              }}
              disabled={registerBYO.isPending}
            />
          </div>

          {/* Inline footer instead of <DialogFooter>: its -mx-4/-mb-4 offsets
              assume the default p-4 DialogContent; with p-0 they push the bar
              outside the dialog (same workaround as CreateAgentDialog). */}
          <div className="flex items-center justify-end gap-2 border-t bg-background px-5 py-3">
            <Button variant="ghost" onClick={closeAddDialog} disabled={registerBYO.isPending}>
              {t(($) => $.tuitui.byo_cancel)}
            </Button>
            <Button
              onClick={handleAddSubmit}
              disabled={!addCanSubmit || registerBYO.isPending}
              data-testid="tuitui-add-submit"
            >
              {registerBYO.isPending
                ? t(($) => $.tuitui.byo_submitting)
                : t(($) => $.tuitui.byo_submit)}
            </Button>
          </div>
        </DialogContent>
      </Dialog>

      <AlertDialog
        open={!!disconnectTarget}
        onOpenChange={(v) => {
          if (!v && !deleteInstallation.isPending) setDisconnectTarget(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(($) => $.tuitui.disconnect_confirm_title)}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.tuitui.disconnect_confirm_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleteInstallation.isPending}>
              {t(($) => $.tuitui.disconnect_confirm_cancel)}
            </AlertDialogCancel>
            <AlertDialogAction
              onClick={handleDisconnect}
              disabled={deleteInstallation.isPending}
              data-testid="tuitui-disconnect-confirm"
            >
              {deleteInstallation.isPending
                ? t(($) => $.tuitui.disconnecting)
                : t(($) => $.tuitui.disconnect)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function TuituiInstallationRow({
  installation,
  canManage,
  onDisconnect,
  groups,
  botIdentity: suppliedBotIdentity,
}: {
  installation: TuituiInstallation;
  canManage: boolean;
  onDisconnect: () => void;
  groups: TuituiGroup[];
  botIdentity?: TuituiGroupBot;
}) {
  const { t, i18n } = useT("settings");
  const { getAgentName } = useActorName();
  const isActive = installation.status === "active";
  const agentAvailable = installation.agent_available !== false;
  const agentName = agentAvailable
    ? getAgentName(installation.agent_id)
    : t(($) => $.tuitui.deleted_agent);
  const linkedIdentityIDs = canManage
    ? (installation.bound_tuitui_user_ids ?? [])
    : [];
  const botIdentity =
    suppliedBotIdentity ?? getTuituiBotIdentity(groups, installation.id);
  return (
    <div className="py-6" data-testid="tuitui-installation-row">
      <div className="flex items-start justify-between gap-6">
        <div className="flex min-w-0 items-start gap-3">
          <ActorAvatar
            actorType="agent"
            actorId={installation.agent_id}
            size="lg"
            enableHoverCard={agentAvailable}
            profileLink={agentAvailable}
          />
          <div className="min-w-0 space-y-1.5">
            <h3 className="truncate text-title-sm font-medium text-pretty">
              {agentName}
              {!isActive && (
                <span className="ml-2 rounded-xs bg-muted px-1.5 py-0.5 text-micro text-muted-foreground">
                  {t(($) => $.tuitui.revoked_badge)}
                </span>
              )}
            </h3>
            <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
              {isActive && (
                <>
                  <TuituiConnectionLabel botName={botIdentity?.bot_name ?? ""} />
                  {linkedIdentityIDs.length > 0 && (
                    <span
                      className="min-w-0 truncate text-micro text-muted-foreground"
                      data-testid="tuitui-installation-identities"
                    >
                      {t(($) => $.tuitui.identity_label, {
                        identity: linkedIdentityIDs.join(", "),
                      })}
                    </span>
                  )}
                  <span
                    className="text-micro text-muted-foreground"
                    aria-hidden="true"
                  >
                    ·
                  </span>
                </>
              )}
              <span
                className="text-micro text-muted-foreground"
                data-testid="tuitui-installation-metadata"
              >
                {t(($) => $.tuitui.installed_at_label, {
                  when: formatInstalledAt(installation.installed_at, i18n.language),
                })}
              </span>
            </div>
          </div>
        </div>
        {canManage && isActive && (
          <Button
            variant="outline"
            size="sm"
            onClick={onDisconnect}
            data-testid="tuitui-installation-disconnect"
          >
            <Trash2 className="h-3 w-3" aria-hidden="true" />
            {t(($) => $.tuitui.disconnect)}
          </Button>
        )}
      </div>
      {isActive && (
        <TuituiBotGroups
          groups={groups}
          installationId={installation.id}
          canForget={canManage}
          showDescription={false}
          className="space-y-3 pt-5"
        />
      )}
    </div>
  );
}

// TuituiAgentBindButton is the per-agent CTA exposed from the agent detail
// page. Tuitui uses the bring-your-own-app model: the button opens a dialog
// where an authorized manager pastes the app_id + app_secret of the Tuitui
// robot application they created (the backend validates both).
// Visibility:
//   1. Only the agent's owner or a workspace owner/admin sees management UI.
//   2. The whole CTA is fail-closed on the deployment flag: a server that
//      does not declare tuitui_supported must never see an actionable Tuitui
//      affordance, even with an old cached installations list.
//   3. If this agent already has an active installation, show the connected
//      badge (already-installed robots stay manageable).
export function TuituiAgentBindButton({
  agentId,
  agentName,
  agentOwnerId,
  botName,
  className,
}: {
  agentId: string;
  agentName?: string;
  /** Mirrors the backend canManageAgent rule for the per-agent entry point. */
  agentOwnerId?: string | null;
  botName?: string;
  className?: string;
}) {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const user = useAuthStore((s) => s.user);
  const tuituiSupported = useConfigStore((s) => s.tuituiSupported);

  const [dialogOpen, setDialogOpen] = useState(false);
  const [baseUrl, setBaseUrl] = useState("");
  const [appId, setAppId] = useState("");
  const [appSecret, setAppSecret] = useState("");
  const [byoError, setByoError] = useState("");
  const registerBYO = useRegisterTuituiBYO(wsId);

  const { data: listing } = useQuery(tuituiInstallationsOptions(wsId));
  const configured = listing?.configured === true;

  const { data: members = [] } = useQuery({
    ...memberListOptions(wsId),
    enabled: !!wsId,
  });
  const currentMember = members.find((m) => m.user_id === user?.id) ?? null;
  const isWorkspaceAdmin =
    currentMember?.role === "owner" || currentMember?.role === "admin";
  const isAgentOwner =
    currentMember != null &&
    !!user?.id &&
    agentOwnerId != null &&
    agentOwnerId === user.id;
  const canManage = isWorkspaceAdmin || isAgentOwner;

  if (!tuituiSupported || !canManage) return null;

  const existing = listing?.installations?.find(
    (inst) => inst.agent_id === agentId && inst.status === "active",
  );
  if (existing) {
    return (
      <TuituiAgentBotConnectedBadge
        installation={existing}
        botName={botName}
        className={className}
      />
    );
  }

  if (!configured) return null;

  function closeDialog() {
    if (registerBYO.isPending) return;
    setDialogOpen(false);
    setBaseUrl("");
    setAppId("");
    setAppSecret("");
    setByoError("");
  }

  async function handleSubmit() {
    const base_url = baseUrl.trim();
    const app_id = appId.trim();
    const app_secret = appSecret.trim();
    // base_url is required like the credentials — never submit an install that
    // would fall through to the server's default target.
    if (registerBYO.isPending || !agentId || !base_url || !app_id || !app_secret) return;
    try {
      await registerBYO.mutateAsync({ agentId, base_url, app_id, app_secret });
      toast.success(t(($) => $.tuitui.byo_success_toast));
      setDialogOpen(false);
      setBaseUrl("");
      setAppId("");
      setAppSecret("");
      setByoError("");
    } catch (e) {
      const message =
        e instanceof Error ? e.message : t(($) => $.tuitui.byo_failed_toast);
      setByoError(message);
      toast.error(message);
    }
  }

  const canSubmit =
    baseUrl.trim() !== "" &&
    appId.trim() !== "" &&
    appSecret.trim() !== "" &&
    !registerBYO.isPending;

  return (
    <div
      className={cn("flex flex-wrap items-center gap-2", className)}
      data-testid="tuitui-agent-bind-buttons"
    >
      <Button
        variant="outline"
        size="sm"
        onClick={() => setDialogOpen(true)}
        disabled={!agentId}
        title={
          agentName
            ? t(($) => $.tuitui.bind_button_title, { agent: agentName })
            : undefined
        }
        data-testid="tuitui-agent-connect"
      >
        <TuituiMark className="h-4 w-4" />
        {t(($) => $.tuitui.bind_button)}
      </Button>

      <Dialog open={dialogOpen} onOpenChange={(v) => (v ? setDialogOpen(true) : closeDialog())}>
        <DialogContent
          className="gap-0 overflow-hidden p-0 sm:max-w-lg"
          data-testid="tuitui-byo-dialog"
        >
          <DialogHeader className="gap-1 border-b px-5 py-3">
            <DialogTitle className="text-title-sm font-semibold">
              {t(($) => $.tuitui.byo_dialog_title)}
            </DialogTitle>
            <p className="text-caption text-muted-foreground">
              {t(($) => $.tuitui.byo_dialog_description)}
            </p>
          </DialogHeader>

          <div className="space-y-4 p-5">
            <TuituiByoFields
              baseUrl={baseUrl}
              appId={appId}
              appSecret={appSecret}
              error={byoError}
              onChange={({ baseUrl: nextUrl, appId: nextId, appSecret: nextSecret }) => {
                setBaseUrl(nextUrl);
                setAppId(nextId);
                setAppSecret(nextSecret);
                // The error described the previous submit attempt.
                setByoError("");
              }}
              disabled={registerBYO.isPending}
            />
          </div>

          <div className="flex items-center justify-end gap-2 border-t bg-background px-5 py-3">
            <Button variant="ghost" onClick={closeDialog} disabled={registerBYO.isPending}>
              {t(($) => $.tuitui.byo_cancel)}
            </Button>
            <Button
              onClick={handleSubmit}
              disabled={!canSubmit}
              data-testid="tuitui-byo-submit"
            >
              {registerBYO.isPending
                ? t(($) => $.tuitui.byo_submitting)
                : t(($) => $.tuitui.byo_submit)}
            </Button>
          </div>
        </DialogContent>
      </Dialog>
    </div>
  );
}

// TuituiAgentBotConnectedBadge is the full "already connected" affordance the
// Integrations tab renders in place of the Connect button: a status row plus a
// soft-destructive Disconnect. Only owners/admins ever reach this component.
function TuituiAgentBotConnectedBadge({
  installation,
  botName,
  className,
}: {
  installation: TuituiInstallation;
  botName?: string;
  className?: string;
}) {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const deleteInstallation = useDeleteTuituiInstallation(wsId);

  const [confirmOpen, setConfirmOpen] = useState(false);

  async function handleDisconnect() {
    if (deleteInstallation.isPending) return;
    try {
      await deleteInstallation.mutateAsync(installation.id);
      toast.success(t(($) => $.tuitui.toast_disconnected));
      setConfirmOpen(false);
    } catch (e) {
      toast.error(
        e instanceof Error ? e.message : t(($) => $.tuitui.toast_disconnect_failed),
      );
    }
  }

  return (
    <div className={cn("space-y-2", className)} data-testid="tuitui-agent-bot-connected">
      <div className="flex items-center justify-between gap-3">
        <TuituiConnectionLabel
          botName={botName}
          showBotIdentity={botName !== undefined}
        />
        <Button
          variant="destructive"
          size="sm"
          onClick={() => setConfirmOpen(true)}
          disabled={deleteInstallation.isPending}
          title={t(($) => $.tuitui.agent_bot_disconnect_tooltip)}
          aria-label={t(($) => $.tuitui.disconnect)}
          data-testid="tuitui-agent-bot-disconnect"
        >
          <Trash2 className="h-3 w-3" aria-hidden="true" />
          {deleteInstallation.isPending
            ? t(($) => $.tuitui.disconnecting)
            : t(($) => $.tuitui.disconnect)}
        </Button>
      </div>

      <AlertDialog
        open={confirmOpen}
        onOpenChange={(v) => {
          if (!v && !deleteInstallation.isPending) setConfirmOpen(false);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(($) => $.tuitui.disconnect_confirm_title)}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.tuitui.disconnect_confirm_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleteInstallation.isPending}>
              {t(($) => $.tuitui.disconnect_confirm_cancel)}
            </AlertDialogCancel>
            <AlertDialogAction onClick={handleDisconnect} disabled={deleteInstallation.isPending}>
              {deleteInstallation.isPending
                ? t(($) => $.tuitui.disconnecting)
                : t(($) => $.tuitui.disconnect)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
