"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { handle } from "@workspace/api/utils";
import { toMemberRole } from "@workspace/auth";
import { useTranslation } from "@workspace/i18n";
import { Button } from "@workspace/ui-web/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui-web/card";
import { Field, FieldError, FieldLabel } from "@workspace/ui-web/field";
import { Input } from "@workspace/ui-web/input";
import { Skeleton } from "@workspace/ui-web/skeleton";
import { toast } from "@workspace/ui-web/toast";

import { api } from "~/lib/api/client";
import { authClient } from "~/lib/auth/client";
import { formatMicroUsd, MICRO_USD, usdToMicroUsd } from "~/modules/keys/money";
import { useActiveOrganization } from "~/modules/organization/hooks/use-active-organization";

interface LowBalanceAlertProps {
  readonly organizationId: string;
}

/** When to email owners and admins that credits are running out (step 10.2). */
export const LowBalanceAlert = ({ organizationId }: LowBalanceAlertProps) => {
  const { t, i18n } = useTranslation(["common", "organization"]);
  const queryClient = useQueryClient();
  const { activeMember } = useActiveOrganization();
  const canUpdate = authClient.organization.checkRolePermission({
    permissions: { billing: ["update"] },
    role: toMemberRole(activeMember?.role),
  });
  const queryKey = ["credits", organizationId, "low-balance-alert"];

  const setting = useQuery({
    queryKey,
    queryFn: () =>
      handle(api.credits[":organizationId"]["low-balance-alert"].$get)({
        param: { organizationId },
      }),
  });
  const [draft, setDraft] = useState<string | null>(null);
  const current = setting.data?.thresholdMicroUsd ?? null;
  const value = draft ?? (current === null ? "" : String(current / MICRO_USD));
  const parsed =
    /^\d+(\.\d{1,2})?$/.test(value.trim()) && Number(value) > 0
      ? Number(value)
      : null;

  const save = useMutation({
    mutationFn: (thresholdMicroUsd: number | null) =>
      handle(api.credits[":organizationId"]["low-balance-alert"].$put)({
        param: { organizationId },
        json: { thresholdMicroUsd },
      }),
    onSuccess: async () => {
      setDraft(null);
      await queryClient.invalidateQueries({ queryKey });
      toast.add({
        title: t("organization:credits.lowBalance.saved"),
        type: "success",
      });
    },
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("organization:credits.lowBalance.title")}</CardTitle>
        <CardDescription>
          {setting.isPending
            ? null
            : current === null
              ? t("organization:credits.lowBalance.off")
              : t("organization:credits.lowBalance.on", {
                  threshold: formatMicroUsd(current, i18n.language),
                })}
        </CardDescription>
      </CardHeader>
      {setting.isPending && (
        <CardContent>
          <Skeleton className="h-10 w-64" />
        </CardContent>
      )}
      {!setting.isPending && canUpdate && (
        <CardContent className="flex flex-wrap items-end gap-4">
          <Field
            data-invalid={value !== "" && parsed === null}
            className="w-48"
          >
            <FieldLabel htmlFor="low-balance-threshold">
              {t("organization:credits.lowBalance.threshold")}
            </FieldLabel>
            <div className="flex items-center gap-2">
              <span className="text-muted-foreground">$</span>
              <Input
                id="low-balance-threshold"
                inputMode="decimal"
                value={value}
                onChange={(event) => setDraft(event.target.value)}
                disabled={save.isPending}
              />
            </div>
            {value !== "" && parsed === null && (
              <FieldError>
                {t("organization:credits.lowBalance.invalid")}
              </FieldError>
            )}
          </Field>
          <Button
            onClick={() =>
              parsed !== null && save.mutate(usdToMicroUsd(parsed))
            }
            disabled={parsed === null || save.isPending}
          >
            {t("common:save")}
          </Button>
          {current !== null && (
            <Button
              variant="outline"
              onClick={() => save.mutate(null)}
              disabled={save.isPending}
            >
              {t("organization:credits.lowBalance.turnOff")}
            </Button>
          )}
        </CardContent>
      )}
    </Card>
  );
};
