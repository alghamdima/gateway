"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { useTranslation } from "@workspace/i18n";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui-web/card";
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

import { OrganizationLimitForm } from "~/modules/admin/account-settings/organization-limit-form";
import { admin } from "~/modules/admin/lib/api";
import { useStaffPermission } from "~/modules/admin/lib/use-staff-permission";

const limitOf = (values: Record<string, unknown>) =>
  typeof values.maxOwnedOrganizations === "number"
    ? values.maxOwnedOrganizations
    : "—";

/** How many organizations a user may own (ADR 0020), with the history of the setting. */
export const AccountSettings = () => {
  const { t, i18n } = useTranslation(["common", "admin"]);
  const queryClient = useQueryClient();
  const settings = useQuery(admin.queries.accountSettings.get());
  const changes = useQuery(admin.queries.accountSettings.getChanges());
  const canEdit = useStaffPermission({ organizations: ["update"] });

  const save = useMutation({
    ...admin.mutations.accountSettings.update,
    onSuccess: async () => {
      await queryClient.invalidateQueries({
        queryKey: admin.queries.accountSettings.get().queryKey,
      });
      toast.add({ title: t("admin:accountSettings.saved"), type: "success" });
    },
  });

  return (
    <div className="@container flex w-full flex-col gap-6">
      <Card>
        <CardHeader>
          <CardTitle>{t("admin:accountSettings.limit.title")}</CardTitle>
          <CardDescription>
            {t("admin:accountSettings.limit.description")}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {settings.data ? (
            <div className="flex flex-col gap-6">
              <p className="text-xl font-semibold tabular-nums">
                {settings.data.maxOwnedOrganizations}
              </p>
              {canEdit && (
                <OrganizationLimitForm
                  id="admin-account-settings-form"
                  value={settings.data.maxOwnedOrganizations}
                  description={t("admin:accountSettings.limit.help")}
                  onSave={(value) =>
                    save.mutateAsync({ maxOwnedOrganizations: value ?? 0 })
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
          <CardTitle>{t("admin:accountSettings.changes.title")}</CardTitle>
          <CardDescription>
            {t("admin:accountSettings.changes.description")}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {!changes.data && <Skeleton className="h-24 w-full" />}
          {changes.data?.length === 0 && (
            <p className="text-muted-foreground text-sm">
              {t("admin:accountSettings.changes.empty")}
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
                    <TableCell className="tabular-nums">
                      {limitOf(change.before)}
                    </TableCell>
                    <TableCell className="tabular-nums">
                      {limitOf(change.after)}
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
