"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";

import { toMemberRole } from "@workspace/auth";
import { useTranslation } from "@workspace/i18n";
import { Badge } from "@workspace/ui-web/badge";
import { Button, buttonVariants } from "@workspace/ui-web/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui-web/card";
import { Icons } from "@workspace/ui-web/icons";
import { Input } from "@workspace/ui-web/input";
import { Progress } from "@workspace/ui-web/progress";
import { Skeleton } from "@workspace/ui-web/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@workspace/ui-web/table";
import { toast } from "@workspace/ui-web/toast";

import { pathsConfig } from "~/config/paths";
import { authClient } from "~/lib/auth/client";
import { TurboLink } from "~/modules/common/turbo-link";
import { ConfirmKeyDialog, SecretModal } from "~/modules/keys/key-dialogs";
import { CreateKeyModal, formatExpiry } from "~/modules/keys/key-form";
import { expiresSoon, KeyStatusBadge } from "~/modules/keys/key-status";
import { keys } from "~/modules/keys/lib/api";
import { formatMicroSar, formatUsage } from "~/modules/keys/money";
import { useNow } from "~/modules/keys/use-now";
import { useActiveOrganization } from "~/modules/organization/hooks/use-active-organization";

import type { KeyAction } from "~/modules/keys/key-dialogs";
import type { CreateKeyResult } from "~/modules/keys/key-form";
import type { KeyListRow } from "~/modules/keys/lib/api";

interface KeysPageProps {
  readonly organizationId: string;
  readonly organizationSlug: string;
}

/** How long typing pauses before the search is sent. */
const SEARCH_DELAY_MS = 300;

/**
 * The organization's keys (KEYS-04, -05, -06): search by name or a pasted
 * key, usage against each limit, last used, expiry, status, and the actions
 * owners and admins may take, each destructive one confirmed.
 */
export const KeysPage = ({
  organizationId,
  organizationSlug,
}: KeysPageProps) => {
  const { t, i18n } = useTranslation(["common", "organization"]);
  const queryClient = useQueryClient();
  const { activeMember } = useActiveOrganization();
  const list = useQuery(keys.queries.list(organizationId));
  const settings = useQuery(keys.queries.settings(organizationId));
  const paths = pathsConfig.dashboard.organization(organizationSlug);

  // members see keys; owners and admins manage them (ADR 0004). The API checks the same.
  const canManage = authClient.organization.checkRolePermission({
    permissions: { apiKey: ["create"] },
    role: toMemberRole(activeMember?.role),
  });

  const [isCreating, setIsCreating] = useState(false);
  const [confirming, setConfirming] = useState<{
    action: KeyAction;
    row: KeyListRow;
  } | null>(null);
  const [secret, setSecret] = useState<string | null>(null);
  const [query, setQuery] = useState("");
  const now = useNow();

  // a pasted key is matched on the server, by its hash; it never goes in a URL
  const search = useMutation(keys.mutations.search);
  const { mutate: runSearch, reset: resetSearch } = search;
  const term = query.trim();
  useEffect(() => {
    if (term === "") {
      resetSearch();
      return;
    }
    const timer = setTimeout(
      () => runSearch({ organizationId, query: term }),
      SEARCH_DELAY_MS,
    );
    return () => clearTimeout(timer);
  }, [term, organizationId, runSearch, resetSearch]);

  const refresh = async () => {
    await queryClient.invalidateQueries({ queryKey: ["keys", organizationId] });
    if (term !== "") runSearch({ organizationId, query: term });
  };

  const create = useMutation({ ...keys.mutations.create, onSuccess: refresh });
  const update = useMutation({ ...keys.mutations.update, onSuccess: refresh });
  const remove = useMutation({ ...keys.mutations.delete, onSuccess: refresh });

  const handleCreate = async (result: CreateKeyResult) => {
    const created = await create.mutateAsync({ organizationId, ...result });
    setIsCreating(false);
    setSecret(created.key);
  };

  const handleConfirm = async () => {
    if (!confirming) return;
    const { action, row } = confirming;
    if (action === "delete") {
      await remove.mutateAsync({ organizationId, keyId: row.id });
    } else {
      await update.mutateAsync({
        organizationId,
        keyId: row.id,
        disabled: true,
      });
    }
    setConfirming(null);
    toast.add({
      title: t(`organization:keys.${action}.success`),
      type: "success",
    });
  };

  const handleEnable = async (row: KeyListRow) => {
    await update.mutateAsync({
      organizationId,
      keyId: row.id,
      disabled: false,
    });
    toast.add({
      title: t("organization:keys.enable.success"),
      type: "success",
    });
  };

  const rows: KeyListRow[] =
    term === "" ? (list.data ?? []) : (search.data ?? []);
  const isLoading = term === "" ? list.isPending : search.isPending;
  const date = (value: string) =>
    new Date(value).toLocaleDateString(i18n.language, {
      dateStyle: "medium",
    });

  return (
    <Card className="w-full">
      <CardHeader className="flex flex-row items-start justify-between gap-4">
        <div className="space-y-1.5">
          <CardTitle>{t("organization:keys.title")}</CardTitle>
          <CardDescription>
            {t("organization:keys.description")}
          </CardDescription>
        </div>
        {canManage && (
          <Button onClick={() => setIsCreating(true)}>
            <Icons.Plus className="size-4" />
            {t("organization:keys.create.cta")}
          </Button>
        )}
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="relative max-w-sm">
          <Icons.Search className="text-muted-foreground absolute top-1/2 left-3 size-4 -translate-y-1/2" />
          <Input
            type="search"
            className="pl-9"
            autoComplete="off"
            spellCheck={false}
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder={t("organization:keys.search.placeholder")}
            aria-label={t("organization:keys.search.label")}
          />
        </div>

        {isLoading && <Skeleton className="h-32 w-full" />}
        {!isLoading && rows.length === 0 && (
          <p className="text-muted-foreground text-sm">
            {term === ""
              ? t("organization:keys.list.empty")
              : t("organization:keys.search.empty")}
          </p>
        )}
        {!isLoading && rows.length > 0 && (
          <>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t("organization:keys.list.key")}</TableHead>
                  <TableHead>{t("organization:keys.list.lastUsed")}</TableHead>
                  <TableHead className="text-right">
                    {t("organization:keys.list.total")}
                  </TableHead>
                  <TableHead>{t("organization:keys.list.limit")}</TableHead>
                  <TableHead>{t("organization:keys.list.status")}</TableHead>
                  <TableHead className="w-0" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((row) => (
                  <TableRow key={row.id}>
                    <TableCell>
                      <TurboLink
                        href={paths.key(row.id)}
                        className="font-medium hover:underline"
                      >
                        {row.name}
                      </TurboLink>
                      <div className="text-muted-foreground font-mono text-xs">
                        {row.prefix}…
                      </div>
                    </TableCell>
                    <TableCell className="whitespace-nowrap">
                      <div>
                        {row.usage.lastUsedAt
                          ? date(row.usage.lastUsedAt)
                          : t("organization:keys.list.never")}
                      </div>
                      <div
                        className={
                          expiresSoon(row.expiresAt, now)
                            ? "flex items-center gap-1 text-xs text-amber-600 dark:text-amber-400"
                            : "text-muted-foreground text-xs"
                        }
                      >
                        {expiresSoon(row.expiresAt, now) && (
                          <Icons.ClockFading className="size-3" />
                        )}
                        {row.expiresAt
                          ? t("organization:keys.list.expires", {
                              date: formatExpiry(
                                new Date(row.expiresAt),
                                i18n.language,
                              ),
                            })
                          : t("organization:keys.list.neverExpires")}
                      </div>
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {formatUsage(row.usage.total, i18n.language)}
                    </TableCell>
                    <TableCell className="min-w-40">
                      <LimitCell row={row} />
                    </TableCell>
                    <TableCell>
                      <KeyStatusBadge status={row.status} />
                    </TableCell>
                    <TableCell>
                      <div className="flex justify-end gap-1">
                        <TurboLink
                          href={paths.key(row.id)}
                          className={buttonVariants({
                            variant: "ghost",
                            size: "sm",
                          })}
                        >
                          {t("organization:keys.list.open")}
                        </TurboLink>
                        {canManage && row.status === "enabled" && (
                          <Button
                            variant="ghost"
                            size="sm"
                            onClick={() =>
                              setConfirming({ action: "disable", row })
                            }
                          >
                            {t("organization:keys.disable.cta")}
                          </Button>
                        )}
                        {canManage && row.status === "disabled" && (
                          <Button
                            variant="ghost"
                            size="sm"
                            disabled={update.isPending}
                            onClick={() => void handleEnable(row)}
                          >
                            {t("organization:keys.enable.cta")}
                          </Button>
                        )}
                        {canManage && (
                          <Button
                            variant="ghost"
                            size="icon"
                            aria-label={t("organization:keys.delete.cta")}
                            onClick={() =>
                              setConfirming({ action: "delete", row })
                            }
                          >
                            <Icons.Trash className="size-4" />
                          </Button>
                        )}
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
            <p className="text-muted-foreground text-sm">
              {t("organization:keys.list.count", { count: rows.length })}
            </p>
          </>
        )}
      </CardContent>

      <CreateKeyModal
        open={isCreating}
        onOpenChange={setIsCreating}
        settings={settings.data}
        onSubmit={handleCreate}
      />
      <SecretModal secret={secret} onClose={() => setSecret(null)} />
      <ConfirmKeyDialog
        action={confirming?.action ?? null}
        name={confirming?.row.name ?? ""}
        isPending={update.isPending || remove.isPending}
        onConfirm={() => void handleConfirm()}
        onClose={() => setConfirming(null)}
      />
    </Card>
  );
};

/** The limit, a bar of what counts against it now, and when it resets. */
const LimitCell = ({ row }: { readonly row: KeyListRow }) => {
  const { t, i18n } = useTranslation("organization");
  if (row.limitMicroSar === null) {
    return (
      <span className="text-muted-foreground">{t("keys.list.noLimit")}</span>
    );
  }
  const used = Math.min(
    100,
    (row.usage.spentAgainstLimit / row.limitMicroSar) * 100,
  );
  return (
    <div className="space-y-1">
      <div className="flex items-center justify-between gap-2">
        <span className="tabular-nums">
          {formatMicroSar(row.limitMicroSar, i18n.language)}
        </span>
        <Badge variant="outline">
          {t(`keys.form.reset.badge.${row.limitReset ?? "never"}`)}
        </Badge>
      </div>
      <Progress value={used} aria-label={t("keys.list.limit")} />
    </div>
  );
};
