"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { MAX_TRANSFER_WAIT_HOURS } from "@workspace/api/schema";
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

const FIELDS = [
  "maxOwnedOrganizations",
  "transferMemberTenureHours",
  "transferCooldownHours",
] as const;

/** How many organizations a user may own and the waits on credit transfers (ADR 0020), with their history. */
export const AccountSettings = () => {
  const { t, i18n } = useTranslation(["common", "admin"]);
  const queryClient = useQueryClient();
  const settings = useQuery(admin.queries.accountSettings.get());
  const changes = useQuery(admin.queries.accountSettings.getChanges());
  const canEdit = useStaffPermission({ organizations: ["update"] });

  // every save sends all three values: the one the form changed over the stored rest
  const current = {
    maxOwnedOrganizations: settings.data?.maxOwnedOrganizations ?? 0,
    transferMemberTenureHours: settings.data?.transferMemberTenureHours ?? 0,
    transferCooldownHours: settings.data?.transferCooldownHours ?? 0,
  };

  const save = useMutation({
    ...admin.mutations.accountSettings.update,
    onSuccess: async () => {
      await queryClient.invalidateQueries({
        queryKey: admin.queries.accountSettings.get().queryKey,
      });
      toast.add({ title: t("admin:accountSettings.saved"), type: "success" });
    },
  });

  // each change records the fields it names; older ones name only the limit
  const describe = (values: Record<string, unknown>) =>
    FIELDS.filter((field) => typeof values[field] === "number")
      .map(
        (field) =>
          `${t(`admin:accountSettings.changes.fields.${field}`)}: ${String(values[field])}`,
      )
      .join(", ");

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
                    save.mutateAsync({
                      ...current,
                      maxOwnedOrganizations: value ?? 0,
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
          <CardTitle>{t("admin:accountSettings.transfers.title")}</CardTitle>
          <CardDescription>
            {t("admin:accountSettings.transfers.description")}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {settings.data ? (
            <div className="grid gap-8 lg:grid-cols-2">
              {(
                ["transferMemberTenureHours", "transferCooldownHours"] as const
              ).map((field) => (
                <div key={field} className="flex flex-col gap-6">
                  <div>
                    <p className="text-muted-foreground text-sm">
                      {t(`admin:accountSettings.transfers.${field}.label`)}
                    </p>
                    <p className="text-xl font-semibold tabular-nums">
                      {t("admin:accountSettings.transfers.hours", {
                        count: settings.data[field],
                      })}
                    </p>
                  </div>
                  {canEdit && (
                    <OrganizationLimitForm
                      id={`admin-account-settings-${field}`}
                      value={settings.data[field]}
                      label={t(
                        `admin:accountSettings.transfers.${field}.label`,
                      )}
                      max={MAX_TRANSFER_WAIT_HOURS}
                      invalidMessage={t(
                        "admin:accountSettings.transfers.invalid",
                      )}
                      description={t(
                        `admin:accountSettings.transfers.${field}.help`,
                      )}
                      onSave={(value) =>
                        save.mutateAsync({ ...current, [field]: value ?? 0 })
                      }
                    />
                  )}
                </div>
              ))}
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
