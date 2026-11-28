"use client";

import { useQuery } from "@tanstack/react-query";

import { useTranslation } from "@workspace/i18n";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui-web/card";
import { Skeleton } from "@workspace/ui-web/skeleton";

import { ChangesList } from "~/modules/admin/key-settings/changes-list";
import { KeySettingsForm } from "~/modules/admin/key-settings/key-settings-form";
import { admin } from "~/modules/admin/lib/api";
import { useStaffPermission } from "~/modules/admin/lib/use-staff-permission";

/** The key defaults and the maximum key lifetime (ADMIN-12), with their history. */
export const KeySettings = () => {
  const { t } = useTranslation(["common", "admin"]);
  const settings = useQuery(admin.queries.keySettings.get());
  const changes = useQuery(admin.queries.keySettings.getChanges());
  const canEdit = useStaffPermission({ pricing: ["update"] });

  return (
    <div className="@container flex w-full flex-col gap-6">
      <Card>
        <CardHeader>
          <CardTitle>{t("admin:keySettings.form.title")}</CardTitle>
          <CardDescription>
            {t("admin:keySettings.form.description")}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {settings.data ? (
            <KeySettingsForm canEdit={canEdit} settings={settings.data} />
          ) : (
            <Skeleton className="h-48 w-full" />
          )}
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>{t("admin:keySettings.changes.title")}</CardTitle>
          <CardDescription>
            {t("admin:keySettings.changes.description")}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {changes.data ? (
            <ChangesList changes={changes.data} />
          ) : (
            <Skeleton className="h-24 w-full" />
          )}
        </CardContent>
      </Card>
    </div>
  );
};
