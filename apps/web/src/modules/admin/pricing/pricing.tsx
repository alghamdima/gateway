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

import { admin } from "~/modules/admin/lib/api";
import { useStaffPermission } from "~/modules/admin/lib/use-staff-permission";
import { ChangesList } from "~/modules/admin/pricing/changes-list";
import { SettingsForm } from "~/modules/admin/pricing/settings-form";

export const Pricing = () => {
  const { t } = useTranslation(["common", "admin"]);
  const pricing = useQuery(admin.queries.pricing.get());
  const changes = useQuery(admin.queries.pricing.getChanges());
  const canEdit = useStaffPermission({ pricing: ["update"] });

  return (
    <div className="@container flex w-full flex-col gap-6">
      <Card>
        <CardHeader>
          <CardTitle>{t("admin:pricing.settings.title")}</CardTitle>
          <CardDescription>
            {t("admin:pricing.settings.description")}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {pricing.data ? (
            <SettingsForm
              canEdit={canEdit}
              topupFeeBps={pricing.data.settings.topupFeeBps}
              topupFeeMinMicroSar={pricing.data.settings.topupFeeMinMicroSar}
              topupMinMicroSar={pricing.data.settings.topupMinMicroSar}
              topupMaxMicroSar={pricing.data.settings.topupMaxMicroSar}
              topupPresetsMicroSar={pricing.data.settings.topupPresetsMicroSar}
              usdToSarRateE6={pricing.data.settings.usdToSarRateE6}
              fxSpreadBps={pricing.data.settings.fxSpreadBps}
            />
          ) : (
            <Skeleton className="h-32 w-full" />
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("admin:pricing.changes.title")}</CardTitle>
          <CardDescription>
            {t("admin:pricing.changes.description")}
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
