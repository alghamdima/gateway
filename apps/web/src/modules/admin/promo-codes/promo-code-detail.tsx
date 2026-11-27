"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { handle } from "@workspace/api/utils";
import { useTranslation } from "@workspace/i18n";
import { Badge } from "@workspace/ui-web/badge";
import { Button } from "@workspace/ui-web/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui-web/card";
import { Skeleton } from "@workspace/ui-web/skeleton";
import { Spinner } from "@workspace/ui-web/spinner";
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
import { api } from "~/lib/api/client";
import { useStaffPermission } from "~/modules/admin/lib/use-staff-permission";
import {
  STATUS_VARIANT,
  usePromoCodeFormat,
} from "~/modules/admin/promo-codes/format";
import { PromoCodeForm } from "~/modules/admin/promo-codes/promo-code-form";
import { TurboLink } from "~/modules/common/turbo-link";

interface PromoCodeDetailProps {
  readonly id: string;
}

/**
 * One promo code (BILL-08): its terms, pause and resume, the edit form, the
 * report (money read from the ledger) and every change with who made it.
 */
export const PromoCodeDetail = ({ id }: PromoCodeDetailProps) => {
  const { t, i18n } = useTranslation(["common", "admin"]);
  const format = usePromoCodeFormat();
  const queryClient = useQueryClient();
  const canEdit = useStaffPermission({ promoCodes: ["update"] });
  const [isEditing, setIsEditing] = useState(false);
  const queryKey = ["admin", "promo-codes", id];

  const code = useQuery({
    queryKey,
    queryFn: () =>
      handle(api.admin["promo-codes"][":id"].$get)({ param: { id } }),
  });
  const report = useQuery({
    queryKey: ["admin", "promo-codes", id, "report"],
    queryFn: () =>
      handle(api.admin["promo-codes"][":id"].report.$get)({ param: { id } }),
  });
  const changes = useQuery({
    queryKey: ["admin", "promo-codes", id, "changes"],
    queryFn: () =>
      handle(api.admin["promo-codes"][":id"].changes.$get)({ param: { id } }),
  });

  const refresh = () =>
    queryClient.invalidateQueries({ queryKey: ["admin", "promo-codes"] });
  const pause = useMutation({
    mutationFn: (isPaused: boolean) =>
      handle(api.admin["promo-codes"][":id"].paused.$put)({
        param: { id },
        json: { isPaused },
      }),
    onSuccess: async (updated) => {
      await refresh();
      toast.add({
        title: t(
          updated.isPaused
            ? "admin:promoCodes.detail.paused"
            : "admin:promoCodes.detail.resumed",
        ),
        type: "success",
      });
    },
  });

  if (!code.data) {
    return <Skeleton className="h-64 w-full" />;
  }
  const data = code.data;
  const when = (value: string) => new Date(value).toLocaleString(i18n.language);
  const figures = report.data
    ? [
        {
          label: t("admin:promoCodes.report.uses"),
          value: String(report.data.uses),
        },
        {
          label: t("admin:promoCodes.report.organizations"),
          value: String(report.data.organizations),
        },
        {
          label: t("admin:promoCodes.report.creditGranted"),
          value: format.money(report.data.creditGrantedMicroSar),
        },
        {
          label: t("admin:promoCodes.report.feeWaived"),
          value: format.money(report.data.feeWaivedMicroSar),
        },
      ]
    : [];

  return (
    <div className="flex w-full flex-col gap-6">
      <TurboLink
        href={pathsConfig.admin.promoCodes.index}
        className="text-muted-foreground text-sm hover:underline"
      >
        {t("admin:promoCodes.detail.back")}
      </TurboLink>

      <Card>
        <CardHeader className="flex flex-wrap items-start justify-between gap-4">
          <div className="flex flex-col gap-1.5">
            <CardTitle className="flex items-center gap-3 font-mono">
              {data.code}
              <Badge variant={STATUS_VARIANT[data.status]}>
                {t(`admin:promoCodes.status.${data.status}`)}
              </Badge>
            </CardTitle>
            <CardDescription>
              {t(`admin:promoCodes.kind.${data.kind}`)} · {format.value(data)}
            </CardDescription>
          </div>
          {canEdit && (
            <div className="flex gap-2">
              <Button
                variant="outline"
                disabled={pause.isPending}
                onClick={() => pause.mutate(!data.isPaused)}
              >
                {pause.isPending ? (
                  <Spinner />
                ) : (
                  t(
                    data.isPaused
                      ? "admin:promoCodes.detail.resume"
                      : "admin:promoCodes.detail.pause",
                  )
                )}
              </Button>
              <Button
                variant={isEditing ? "secondary" : "default"}
                onClick={() => setIsEditing(!isEditing)}
              >
                {isEditing
                  ? t("common:cancel")
                  : t("admin:promoCodes.detail.edit")}
              </Button>
            </div>
          )}
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          {isEditing ? (
            <PromoCodeForm
              code={data}
              isKindLocked={data.uses > 0}
              onSaved={() => setIsEditing(false)}
            />
          ) : (
            <dl className="grid gap-4 sm:grid-cols-3">
              <Term label={t("admin:promoCodes.list.uses")}>
                {format.uses(data)}
              </Term>
              <Term label={t("admin:promoCodes.list.validity")}>
                {format.validity(data)}
              </Term>
              <Term label={t("admin:promoCodes.detail.terms")}>
                <span className="flex flex-col">
                  <span>
                    {t(
                      data.onePerOrganization
                        ? "admin:promoCodes.detail.onePerOrganization"
                        : "admin:promoCodes.detail.repeatable",
                    )}
                  </span>
                  {data.minTopupMicroSar !== null && (
                    <span>
                      {t("admin:promoCodes.detail.minTopup", {
                        amount: format.money(data.minTopupMicroSar),
                      })}
                    </span>
                  )}
                  <span>
                    {data.allOrganizations
                      ? t("admin:promoCodes.detail.organizationsAll")
                      : data.organizations.map((org) => org.name).join(", ")}
                  </span>
                </span>
              </Term>
              {data.note && (
                <p className="text-muted-foreground text-sm sm:col-span-3">
                  {data.note}
                </p>
              )}
            </dl>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("admin:promoCodes.report.title")}</CardTitle>
          <CardDescription>
            {t("admin:promoCodes.report.description")}
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-6">
          {!report.data ? (
            <Skeleton className="h-32 w-full" />
          ) : (
            <>
              <dl className="grid gap-4 sm:grid-cols-4">
                {figures.map((figure) => (
                  <Term key={figure.label} label={figure.label}>
                    <span className="text-2xl font-semibold tabular-nums">
                      {figure.value}
                    </span>
                  </Term>
                ))}
              </dl>
              {report.data.redemptions.length === 0 ? (
                <p className="text-muted-foreground text-sm">
                  {t("admin:promoCodes.report.empty")}
                </p>
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>{t("admin:promoCodes.report.when")}</TableHead>
                      <TableHead>
                        {t("admin:promoCodes.report.organization")}
                      </TableHead>
                      <TableHead className="text-right">
                        {t("admin:promoCodes.report.topUp")}
                      </TableHead>
                      <TableHead className="text-right">
                        {t("admin:promoCodes.report.discount")}
                      </TableHead>
                      <TableHead className="text-right">
                        {t("admin:promoCodes.report.credit")}
                      </TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {report.data.redemptions.map((row) => (
                      <TableRow key={row.id}>
                        <TableCell className="text-muted-foreground whitespace-nowrap">
                          {when(row.createdAt)}
                        </TableCell>
                        <TableCell>
                          <TurboLink
                            href={pathsConfig.admin.organizations.organization(
                              row.organizationId,
                            )}
                            className="hover:underline"
                          >
                            {row.organizationName ?? row.organizationId}
                          </TurboLink>
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {row.topupCreditMicroSar === null
                            ? "—"
                            : format.money(row.topupCreditMicroSar)}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {format.money(row.feeDiscountMicroSar)}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {format.money(row.creditMicroSar)}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}
            </>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("admin:promoCodes.changes.title")}</CardTitle>
        </CardHeader>
        <CardContent>
          {!changes.data ? (
            <Skeleton className="h-24 w-full" />
          ) : changes.data.length === 0 ? (
            <p className="text-muted-foreground text-sm">
              {t("admin:promoCodes.changes.empty")}
            </p>
          ) : (
            <ul className="flex flex-col gap-3 text-sm">
              {changes.data.map((change) => (
                <li key={change.id} className="flex flex-col gap-1">
                  <span className="text-muted-foreground">
                    {when(change.changedAt)}{" "}
                    {t("admin:promoCodes.changes.by", {
                      name:
                        change.changedByName ??
                        change.changedByEmail ??
                        change.changedBy,
                    })}
                  </span>
                  <span className="font-mono text-xs break-all">
                    {change.before === null
                      ? t("admin:promoCodes.changes.created")
                      : describeChange(change.before, change.after)}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      </Card>
    </div>
  );
};

const Term = ({
  label,
  children,
}: {
  readonly label: string;
  readonly children: React.ReactNode;
}) => (
  <div>
    <dt className="text-muted-foreground text-sm">{label}</dt>
    <dd className="font-medium">{children}</dd>
  </div>
);

/** The fields a change moved, as "field: before → after". */
const describeChange = (
  before: Record<string, unknown>,
  after: Record<string, unknown> | null,
) =>
  Object.keys({ ...before, ...after })
    .filter(
      (field) =>
        JSON.stringify(before[field]) !== JSON.stringify(after?.[field]),
    )
    .map(
      (field) =>
        `${field}: ${JSON.stringify(before[field] ?? null)} → ${JSON.stringify(after?.[field] ?? null)}`,
    )
    .join("; ");
