"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { MAX_MIN_CALLS_FOR_STATS } from "@workspace/api/schema";
import { useTranslation } from "@workspace/i18n";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui-web/card";
import { Label } from "@workspace/ui-web/label";
import { Skeleton } from "@workspace/ui-web/skeleton";
import { Switch } from "@workspace/ui-web/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@workspace/ui-web/table";
import { toast } from "@workspace/ui-web/toast";

import { OrganizationLimitForm } from "~/modules/admin/account-settings/organization-limit-form";
import { admin } from "~/modules/admin/lib/api";
import { useStaffPermission } from "~/modules/admin/lib/use-staff-permission";

/**
 * What the catalog's figures need before they are shown, and whether traffic
 * volume is public (CAT-11), with the history of both.
 */
export const CatalogSettings = () => {
  const { t, i18n } = useTranslation(["common", "admin"]);
  const queryClient = useQueryClient();
  const settings = useQuery(admin.queries.catalogSettings.get());
  const changes = useQuery(admin.queries.catalogSettings.getChanges());
  const canEdit = useStaffPermission({ pricing: ["update"] });

  // every save sends both values: the one changed over the stored other
  const current = {
    minCallsForStats: settings.data?.minCallsForStats ?? 1,
    isTrafficPublic: settings.data?.isTrafficPublic ?? false,
  };
  const save = useMutation({
    ...admin.mutations.catalogSettings.update,
    onSuccess: async () => {
      await queryClient.invalidateQueries({
        queryKey: admin.queries.catalogSettings.get().queryKey,
      });
      toast.add({ title: t("admin:catalogSettings.saved"), type: "success" });
    },
  });

  const describe = (values: Record<string, unknown>) =>
    [
      typeof values.minCallsForStats === "number"
        ? `${t("admin:catalogSettings.changes.fields.minCallsForStats")}: ${values.minCallsForStats}`
        : null,
      typeof values.isTrafficPublic === "boolean"
        ? `${t("admin:catalogSettings.changes.fields.isTrafficPublic")}: ${t(values.isTrafficPublic ? "admin:catalogSettings.changes.public" : "admin:catalogSettings.changes.hidden")}`
        : null,
    ]
      .filter(Boolean)
      .join(", ");

  return (
    <div className="@container flex w-full flex-col gap-6">
      <Card>
        <CardHeader>
          <CardTitle>{t("admin:catalogSettings.minCalls.title")}</CardTitle>
          <CardDescription>
            {t("admin:catalogSettings.minCalls.description")}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {settings.data ? (
            <div className="flex flex-col gap-6">
              <p className="text-xl font-semibold tabular-nums">
                {settings.data.minCallsForStats}
              </p>
              {canEdit && (
                <OrganizationLimitForm
                  id="admin-catalog-min-calls-form"
                  value={settings.data.minCallsForStats}
                  label={t("admin:catalogSettings.minCalls.label")}
                  max={MAX_MIN_CALLS_FOR_STATS}
                  invalidMessage={t("admin:catalogSettings.minCalls.invalid")}
                  description={t("admin:catalogSettings.minCalls.help")}
                  onSave={(value) =>
                    save.mutateAsync({
                      ...current,
                      minCallsForStats: Math.max(1, value ?? 1),
                    })
                  }
                />
              )}
            </div>
          ) : (
            <Skeleton className="h-24 w-full" />
          )}
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>{t("admin:catalogSettings.traffic.title")}</CardTitle>
          <CardDescription>
            {t("admin:catalogSettings.traffic.description")}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {settings.data ? (
            <div className="flex items-center gap-3">
              <Switch
                id="admin-catalog-traffic-public"
                checked={settings.data.isTrafficPublic}
                disabled={!canEdit || save.isPending}
                onCheckedChange={(checked) =>
                  save.mutate({ ...current, isTrafficPublic: checked })
                }
              />
              <Label htmlFor="admin-catalog-traffic-public">
                {t("admin:catalogSettings.traffic.label")}
              </Label>
            </div>
          ) : (
            <Skeleton className="h-8 w-full" />
          )}
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>{t("admin:catalogSettings.changes.title")}</CardTitle>
          <CardDescription>
            {t("admin:catalogSettings.changes.description")}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {!changes.data && <Skeleton className="h-24 w-full" />}
          {changes.data?.length === 0 && (
            <p className="text-muted-foreground text-sm">
              {t("admin:catalogSettings.changes.empty")}
            </p>
          )}
          {changes.data && changes.data.length > 0 && (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t("admin:keySettings.changes.when")}</TableHead>
                  <TableHead>{t("admin:keySettings.changes.who")}</TableHead>
                  <TableHead>{t("admin:keySettings.changes.before")}</TableHead>
                  <TableHead>{t("admin:keySettings.changes.after")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {changes.data.map((change) => (
                  <TableRow key={change.id}>
                    <TableCell className="text-muted-foreground whitespace-nowrap">
                      {new Date(change.changedAt).toLocaleString(i18n.language)}
                    </TableCell>
                    <TableCell>
                      {change.changedByName ?? change.changedByEmail ?? "—"}
                    </TableCell>
                    <TableCell className="text-sm whitespace-normal">
                      {describe(change.before)}
                    </TableCell>
                    <TableCell className="text-sm whitespace-normal">
                      {describe(change.after)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </div>
  );
};
