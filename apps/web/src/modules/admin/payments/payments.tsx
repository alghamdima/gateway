"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { useTranslation } from "@workspace/i18n";
import { Alert, AlertDescription } from "@workspace/ui-web/alert";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui-web/card";
import { Icons } from "@workspace/ui-web/icons";
import { Label } from "@workspace/ui-web/label";
import { RadioGroup } from "@workspace/ui-web/radio-group";
import { Skeleton } from "@workspace/ui-web/skeleton";
import { Switch } from "@workspace/ui-web/switch";
import { toast } from "@workspace/ui-web/toast";

import { admin } from "~/modules/admin/lib/api";
import { useStaffPermission } from "~/modules/admin/lib/use-staff-permission";
import { ChangesList } from "~/modules/admin/payments/changes-list";
import { ProviderRow } from "~/modules/admin/payments/provider-row";

import type { PaymentsOverview } from "~/modules/admin/payments/types";

/**
 * Payment gateway management (ADMIN-03): top-ups on or off, the active
 * provider, each provider's switch and mode. The API refuses what would leave
 * customers without a working provider; secrets are shown as set or missing.
 */
export const Payments = () => {
  const { t } = useTranslation(["common", "admin"]);
  const queryClient = useQueryClient();
  const overview = useQuery(admin.queries.payments.get());
  const changes = useQuery(admin.queries.payments.getChanges());
  const canEdit = useStaffPermission({ payments: ["update"] });

  const onSaved = async () => {
    await queryClient.invalidateQueries({ queryKey: ["admin", "payments"] });
    toast.add({ title: t("admin:payments.saved"), type: "success" });
  };
  const updateSettings = useMutation({
    ...admin.mutations.payments.updateSettings,
    onSuccess: onSaved,
  });
  const updateProvider = useMutation({
    ...admin.mutations.payments.updateProvider,
    onSuccess: onSaved,
  });

  const data = overview.data;
  const isSaving = updateSettings.isPending || updateProvider.isPending;
  const reason = data ? unavailableReason(data) : null;

  return (
    <div className="@container flex w-full flex-col gap-6">
      <Card>
        <CardHeader>
          <CardTitle>{t("admin:payments.topups.title")}</CardTitle>
          <CardDescription>
            {t("admin:payments.topups.description")}
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          {data ? (
            <>
              <div className="flex items-center gap-3">
                <Switch
                  id="admin-payments-topups"
                  checked={data.topupsEnabled}
                  disabled={!canEdit || isSaving}
                  onCheckedChange={(checked) =>
                    updateSettings.mutate({
                      topupsEnabled: checked,
                      activeProvider: data.activeProvider,
                    })
                  }
                />
                <Label htmlFor="admin-payments-topups">
                  {t("admin:payments.topups.label")}
                </Label>
              </div>
              {reason && (
                <Alert>
                  <Icons.TriangleAlert />
                  <AlertDescription>
                    {t("admin:payments.topups.unavailable", {
                      reason:
                        reason.kind === "secrets"
                          ? t("admin:payments.topups.reason.secrets", {
                              mode: t(`admin:payments.mode.${reason.mode}`),
                            })
                          : t(`admin:payments.topups.reason.${reason.kind}`),
                    })}
                  </AlertDescription>
                </Alert>
              )}
            </>
          ) : (
            <Skeleton className="h-10 w-64" />
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("admin:payments.providers.title")}</CardTitle>
          <CardDescription>
            {t("admin:payments.providers.description")}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {data ? (
            <RadioGroup
              value={data.activeProvider}
              disabled={!canEdit || isSaving}
              onValueChange={(value) => {
                const provider = data.providers.find(
                  (row) => row.provider === value,
                );
                if (provider) {
                  updateSettings.mutate({
                    topupsEnabled: data.topupsEnabled,
                    activeProvider: provider.provider,
                  });
                }
              }}
              className="gap-4"
            >
              {data.providers.map((provider) => (
                <ProviderRow
                  key={provider.provider}
                  provider={provider}
                  canEdit={canEdit}
                  isSaving={isSaving}
                  onChange={(values) =>
                    updateProvider.mutate({
                      provider: provider.provider,
                      ...values,
                    })
                  }
                />
              ))}
            </RadioGroup>
          ) : (
            <Skeleton className="h-48 w-full" />
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("admin:payments.changes.title")}</CardTitle>
          <CardDescription>
            {t("admin:payments.changes.description")}
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

type UnavailableReason =
  | { kind: "off" | "disabled" }
  | { kind: "secrets"; mode: "test" | "live" };

/** Why customers cannot top up now, by the same rules as the API; null when they can. */
const unavailableReason = (
  data: PaymentsOverview,
): UnavailableReason | null => {
  if (!data.topupsEnabled) {
    return { kind: "off" };
  }
  const active = data.providers.find((row) => row.isActive);
  if (!active?.enabled) {
    return { kind: "disabled" };
  }
  if (!active.secrets?.[active.mode].isConfigured) {
    return { kind: "secrets", mode: active.mode };
  }
  return null;
};
