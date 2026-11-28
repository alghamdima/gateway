"use client";

import { standardSchemaResolver } from "@hookform/resolvers/standard-schema";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useRouter } from "next/navigation";
import { useState } from "react";
import { Controller, useForm } from "react-hook-form";
import { Bar, BarChart, CartesianGrid, XAxis, YAxis } from "recharts";
import * as z from "zod";

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
import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
} from "@workspace/ui-web/chart";
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
} from "@workspace/ui-web/field";
import { Icons } from "@workspace/ui-web/icons";
import { Input } from "@workspace/ui-web/input";
import { Skeleton } from "@workspace/ui-web/skeleton";
import { Spinner } from "@workspace/ui-web/spinner";
import { toast } from "@workspace/ui-web/toast";

import { pathsConfig } from "~/config/paths";
import { authClient } from "~/lib/auth/client";
import { TurboLink } from "~/modules/common/turbo-link";
import { ConfirmKeyDialog } from "~/modules/keys/key-dialogs";
import { formatExpiry } from "~/modules/keys/key-form";
import { KeyStatusBadge } from "~/modules/keys/key-status";
import { keys } from "~/modules/keys/lib/api";
import {
  LimitFields,
  limitValuesSchema,
  toKeyLimit,
  toLimitValues,
} from "~/modules/keys/limit-fields";
import { formatMicroSar, formatUsage, MICRO_SAR } from "~/modules/keys/money";
import { useActiveOrganization } from "~/modules/organization/hooks/use-active-organization";

import type { ChartConfig } from "@workspace/ui-web/chart";
import type { KeyAction } from "~/modules/keys/key-dialogs";
import type { KeyDetail } from "~/modules/keys/lib/api";

interface KeyPageProps {
  readonly organizationId: string;
  readonly organizationSlug: string;
  readonly keyId: string;
}

/**
 * One key's page (KEYS-11), as OpenRouter's: name and limit editable, the
 * expiry read-only, spend over 30 days, usage by period, last used, the
 * budget against the limit, and the danger zone.
 */
export const KeyPage = ({
  organizationId,
  organizationSlug,
  keyId,
}: KeyPageProps) => {
  const { t, i18n } = useTranslation(["common", "organization"]);
  const router = useRouter();
  const queryClient = useQueryClient();
  const { activeMember } = useActiveOrganization();
  const detail = useQuery(keys.queries.detail(organizationId, keyId));
  const settings = useQuery(keys.queries.settings(organizationId));
  const paths = pathsConfig.dashboard.organization(organizationSlug);
  const role = toMemberRole(activeMember?.role);

  // the same rules as the API: members read, owners and admins manage (ADR 0004)
  const canManage = authClient.organization.checkRolePermission({
    permissions: { apiKey: ["update"] },
    role,
  });
  const canSeeActivity = authClient.organization.checkRolePermission({
    permissions: { billing: ["read"] },
    role,
  });

  const [confirming, setConfirming] = useState<KeyAction | null>(null);
  const refresh = () =>
    queryClient.invalidateQueries({ queryKey: ["keys", organizationId] });
  const update = useMutation({ ...keys.mutations.update, onSuccess: refresh });
  const remove = useMutation(keys.mutations.delete);

  if (detail.isPending) {
    return <Skeleton className="h-96 w-full" />;
  }
  const key = detail.data;
  if (!key) {
    return (
      <Card className="w-full">
        <CardHeader>
          <CardTitle>{t("organization:keys.page.notFound")}</CardTitle>
        </CardHeader>
        <CardContent>
          <TurboLink
            href={paths.keys}
            className={buttonVariants({ variant: "outline" })}
          >
            {t("organization:keys.page.back")}
          </TurboLink>
        </CardContent>
      </Card>
    );
  }

  const handleConfirm = async () => {
    if (confirming === "delete") {
      await remove.mutateAsync({ organizationId, keyId });
      toast.add({
        title: t("organization:keys.delete.success"),
        type: "success",
      });
      await refresh();
      router.push(paths.keys);
      return;
    }
    await update.mutateAsync({ organizationId, keyId, disabled: true });
    setConfirming(null);
    toast.add({
      title: t("organization:keys.disable.success"),
      type: "success",
    });
  };

  const handleEnable = async () => {
    await update.mutateAsync({ organizationId, keyId, disabled: false });
    toast.add({
      title: t("organization:keys.enable.success"),
      type: "success",
    });
  };

  const longDate = (value: string) =>
    new Date(value).toLocaleDateString(i18n.language, { dateStyle: "medium" });

  return (
    <div className="flex w-full flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="flex flex-col gap-2">
          <TurboLink
            href={paths.keys}
            className="text-muted-foreground flex items-center gap-1 text-sm hover:underline"
          >
            <Icons.ChevronLeft className="size-4" />
            {t("organization:keys.page.back")}
          </TurboLink>
          <div className="flex flex-wrap items-center gap-3">
            <h1 className="text-2xl font-semibold">{key.name}</h1>
            <KeyStatusBadge status={key.status} />
          </div>
          <div className="text-muted-foreground flex flex-wrap items-center gap-3 text-sm">
            <span className="flex items-center gap-1 font-mono">
              <Icons.Key className="size-4" />
              {key.prefix}…
            </span>
            <span>
              {t("organization:keys.page.created", {
                date: longDate(key.createdAt),
              })}
            </span>
          </div>
        </div>
        {canSeeActivity && (
          <TurboLink
            href={paths.activity}
            className={buttonVariants({ variant: "outline", size: "sm" })}
          >
            <Icons.TrendingUp className="size-4" />
            {t("organization:keys.page.activity")}
          </TurboLink>
        )}
      </div>

      {key.status === "expired" && (
        <p className="text-destructive text-sm">
          {t("organization:keys.page.expiredNote")}
        </p>
      )}

      <DetailsCard
        organizationId={organizationId}
        apiKey={key}
        presets={settings.data?.limitPresetsMicroSar ?? []}
        canManage={canManage}
      />

      <SpendCard spendByDay={key.spendByDay} />

      <div className="grid gap-6 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>{t("organization:keys.page.usage")}</CardTitle>
            <CardDescription>
              {t("organization:keys.page.usageDescription")}
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-6">
            <dl className="grid grid-cols-2 gap-4 sm:grid-cols-4">
              {(
                [
                  ["total", key.usage.total],
                  ["today", key.usage.daily],
                  ["week", key.usage.weekly],
                  ["month", key.usage.monthly],
                ] as const
              ).map(([label, value]) => (
                <div key={label}>
                  <dt className="text-muted-foreground text-sm">
                    {t(`organization:keys.page.${label}`)}
                  </dt>
                  <dd className="font-medium tabular-nums">
                    {formatUsage(value, i18n.language)}
                  </dd>
                </div>
              ))}
            </dl>
            <div>
              <h3 className="mb-2 text-sm font-medium">
                {t("organization:keys.page.metadata")}
              </h3>
              <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-1 text-sm">
                <dt className="text-muted-foreground">
                  {t("organization:keys.page.createdAt")}
                </dt>
                <dd>{formatExpiry(new Date(key.createdAt), i18n.language)}</dd>
                <dt className="text-muted-foreground">
                  {t("organization:keys.page.lastUsed")}
                </dt>
                <dd>
                  {key.usage.lastUsedAt
                    ? formatExpiry(
                        new Date(key.usage.lastUsedAt),
                        i18n.language,
                      )
                    : t("organization:keys.page.lastUsedNever")}
                </dd>
              </dl>
            </div>
          </CardContent>
        </Card>
        <BudgetCard apiKey={key} />
      </div>

      {canManage && (
        <Card className="border-destructive/50">
          <CardHeader>
            <CardTitle className="text-destructive">
              {t("organization:keys.page.danger")}
            </CardTitle>
          </CardHeader>
          <CardContent className="divide-y">
            {key.status !== "expired" && (
              <DangerRow
                title={
                  key.status === "disabled"
                    ? t("organization:keys.page.enableTitle")
                    : t("organization:keys.page.disableTitle")
                }
                description={
                  key.status === "disabled"
                    ? t("organization:keys.page.enableDescription")
                    : t("organization:keys.page.disableDescription")
                }
              >
                {key.status === "disabled" ? (
                  <Button
                    variant="outline"
                    disabled={update.isPending}
                    onClick={() => void handleEnable()}
                  >
                    {t("organization:keys.enable.cta")}
                  </Button>
                ) : (
                  <Button
                    variant="outline"
                    className="text-destructive"
                    onClick={() => setConfirming("disable")}
                  >
                    {t("organization:keys.disable.cta")}
                  </Button>
                )}
              </DangerRow>
            )}
            <DangerRow
              title={t("organization:keys.page.deleteTitle")}
              description={t("organization:keys.page.deleteDescription")}
            >
              <Button
                variant="destructive"
                onClick={() => setConfirming("delete")}
              >
                {t("organization:keys.delete.cta")}
              </Button>
            </DangerRow>
          </CardContent>
        </Card>
      )}

      <ConfirmKeyDialog
        action={confirming}
        name={key.name}
        isPending={update.isPending || remove.isPending}
        onConfirm={() => void handleConfirm()}
        onClose={() => setConfirming(null)}
      />
    </div>
  );
};

const DangerRow = ({
  title,
  description,
  children,
}: {
  readonly title: string;
  readonly description: string;
  readonly children: React.ReactNode;
}) => (
  <div className="flex flex-wrap items-center justify-between gap-4 py-4 first:pt-0 last:pb-0">
    <div>
      <p className="text-destructive font-medium">{title}</p>
      <p className="text-muted-foreground text-sm">{description}</p>
    </div>
    {children}
  </div>
);

/** Name and limit, editable; the expiry shown, never editable (KEYS-03). */
const DetailsCard = ({
  organizationId,
  apiKey,
  presets,
  canManage,
}: {
  readonly organizationId: string;
  readonly apiKey: KeyDetail;
  readonly presets: readonly number[];
  readonly canManage: boolean;
}) => {
  const { t, i18n } = useTranslation(["common", "organization"]);
  const queryClient = useQueryClient();
  const update = useMutation({
    ...keys.mutations.update,
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: ["keys", organizationId] }),
  });

  const schema = z.object({
    name: z.string().trim().min(1).max(100),
    limit: limitValuesSchema(t("organization:keys.form.limit.invalid")),
  });
  const form = useForm({
    resolver: standardSchemaResolver(schema),
    values: {
      name: apiKey.name,
      limit: toLimitValues(apiKey.limitMicroSar, apiKey.limitReset, presets),
    },
  });

  const submit = async (values: z.infer<typeof schema>) => {
    await update.mutateAsync({
      organizationId,
      keyId: apiKey.id,
      name: values.name,
      limit: toKeyLimit(values.limit),
    });
    toast.add({ title: t("organization:keys.edit.success"), type: "success" });
  };
  const isDisabled = !canManage || form.formState.isSubmitting;

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("organization:keys.page.details")}</CardTitle>
      </CardHeader>
      <CardContent>
        <form
          id="key-details-form"
          noValidate
          onSubmit={form.handleSubmit(submit)}
          className="grid gap-6 md:grid-cols-2"
        >
          <div className="space-y-6">
            <Controller
              name="name"
              control={form.control}
              render={({ field, fieldState }) => (
                <Field data-invalid={fieldState.invalid}>
                  <FieldLabel htmlFor="key-details-name">
                    {t("organization:keys.form.name.label")}
                  </FieldLabel>
                  <Input
                    id="key-details-name"
                    autoComplete="off"
                    {...field}
                    aria-invalid={fieldState.invalid}
                    disabled={isDisabled}
                  />
                  <FieldDescription>
                    {t("organization:keys.form.name.description")}
                  </FieldDescription>
                  {fieldState.invalid && (
                    <FieldError errors={[fieldState.error]} />
                  )}
                </Field>
              )}
            />
            <Field>
              <FieldLabel htmlFor="key-details-expiry">
                {t("organization:keys.form.expiry.label")}
              </FieldLabel>
              <Input
                id="key-details-expiry"
                readOnly
                disabled
                value={
                  apiKey.expiresAt
                    ? formatExpiry(new Date(apiKey.expiresAt), i18n.language)
                    : t("organization:keys.form.expiry.neverValue")
                }
              />
              <FieldDescription>
                {t("organization:keys.form.expiry.readOnly")}
              </FieldDescription>
            </Field>
          </div>
          <Controller
            name="limit"
            control={form.control}
            render={({ field }) => (
              <div className="space-y-2">
                <LimitFields
                  idPrefix="key-details"
                  value={field.value}
                  onChange={field.onChange}
                  error={form.formState.errors.limit?.limitSar?.message}
                  presets={presets}
                  disabled={isDisabled}
                />
                <FieldDescription>
                  {t("organization:keys.form.limit.description")}
                </FieldDescription>
              </div>
            )}
          />
        </form>
        {canManage && (
          <Button
            type="submit"
            form="key-details-form"
            className="mt-6"
            disabled={isDisabled}
          >
            {form.formState.isSubmitting ? <Spinner /> : t("save")}
          </Button>
        )}
      </CardContent>
    </Card>
  );
};

/** Spend per UTC day over the last 30 days. */
const SpendCard = ({
  spendByDay,
}: {
  readonly spendByDay: KeyDetail["spendByDay"];
}) => {
  const { t, i18n } = useTranslation(["common", "organization"]);
  const total = spendByDay.reduce((sum, day) => sum + day.spend, 0);
  const dayLabel = new Intl.DateTimeFormat(i18n.language, {
    month: "short",
    day: "numeric",
    timeZone: "UTC",
  });
  const chartConfig = {
    spend: {
      label: t("organization:keys.page.spend"),
      color: "var(--chart-1)",
    },
  } satisfies ChartConfig;

  return (
    <Card>
      <CardHeader>
        <CardDescription>{t("organization:keys.page.spend")}</CardDescription>
        <CardTitle className="text-2xl tabular-nums">
          {formatMicroSar(total, i18n.language)}
        </CardTitle>
      </CardHeader>
      <CardContent>
        {total === 0 ? (
          <p className="text-muted-foreground py-12 text-center text-sm">
            {t("organization:keys.page.noSpend")}
          </p>
        ) : (
          <ChartContainer config={chartConfig} className="h-48 w-full">
            <BarChart
              accessibilityLayer
              data={spendByDay.map((day) => ({
                day: day.day,
                spend: day.spend / MICRO_SAR,
              }))}
            >
              <CartesianGrid vertical={false} />
              <XAxis
                dataKey="day"
                tickLine={false}
                axisLine={false}
                minTickGap={24}
                tickFormatter={(value: string) =>
                  dayLabel.format(new Date(value))
                }
              />
              <YAxis tickLine={false} axisLine={false} width={56} />
              <ChartTooltip
                content={
                  <ChartTooltipContent
                    labelFormatter={(value) =>
                      dayLabel.format(new Date(String(value)))
                    }
                    formatter={(value) =>
                      formatUsage(Number(value) * MICRO_SAR, i18n.language)
                    }
                  />
                }
              />
              <Bar dataKey="spend" fill="var(--color-spend)" radius={4} />
            </BarChart>
          </ChartContainer>
        )}
      </CardContent>
    </Card>
  );
};

/** "SAR x / SAR y" with a ring of how much of the limit is used now. */
const BudgetCard = ({ apiKey }: { readonly apiKey: KeyDetail }) => {
  const { t, i18n } = useTranslation(["common", "organization"]);
  const money = (microSar: number) => formatMicroSar(microSar, i18n.language);
  const limit = apiKey.limitMicroSar;
  const used = apiKey.usage.spentAgainstLimit;
  const share = limit === null ? 0 : Math.min(1, used / limit);

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("organization:keys.page.budget")}</CardTitle>
      </CardHeader>
      <CardContent>
        {limit === null ? (
          <p className="text-muted-foreground text-sm">
            {t("organization:keys.page.budgetNone")}
          </p>
        ) : (
          <div className="flex items-center gap-4">
            <BudgetRing share={share} />
            <div className="space-y-1">
              <p className="text-muted-foreground text-sm">
                {t("organization:keys.page.budgetLabel")}
              </p>
              <p className="flex items-center gap-2 font-medium tabular-nums">
                {t("organization:keys.page.budgetUsed", {
                  used: money(used),
                  limit: money(limit),
                })}
                <Badge variant="outline">
                  {t(
                    `organization:keys.form.reset.badge.${apiKey.limitReset ?? "never"}`,
                  )}
                </Badge>
              </p>
              <p className="text-muted-foreground text-sm">
                {t("organization:keys.page.budgetRemaining", {
                  remaining: money(Math.max(0, limit - used)),
                })}
              </p>
              {apiKey.usage.resetsAt && (
                <p className="text-muted-foreground text-sm">
                  {t("organization:keys.page.budgetResets", {
                    date: formatExpiry(
                      new Date(apiKey.usage.resetsAt),
                      i18n.language,
                    ),
                  })}
                </p>
              )}
            </div>
          </div>
        )}
      </CardContent>
    </Card>
  );
};

const RING_RADIUS = 26;
const RING_LENGTH = 2 * Math.PI * RING_RADIUS;

const BudgetRing = ({ share }: { readonly share: number }) => {
  const { i18n } = useTranslation();
  const percent = new Intl.NumberFormat(i18n.language, {
    style: "percent",
    maximumFractionDigits: 0,
  }).format(share);
  return (
    <svg
      viewBox="0 0 64 64"
      className="size-16 shrink-0"
      role="img"
      aria-label={percent}
    >
      <circle
        cx="32"
        cy="32"
        r={RING_RADIUS}
        fill="none"
        strokeWidth="6"
        className="stroke-muted"
      />
      <circle
        cx="32"
        cy="32"
        r={RING_RADIUS}
        fill="none"
        strokeWidth="6"
        strokeLinecap="round"
        strokeDasharray={RING_LENGTH}
        strokeDashoffset={RING_LENGTH * (1 - share)}
        transform="rotate(-90 32 32)"
        className={share >= 1 ? "stroke-destructive" : "stroke-primary"}
      />
      <text
        x="32"
        y="36"
        textAnchor="middle"
        className="fill-foreground text-[11px] font-medium"
      >
        {percent}
      </text>
    </svg>
  );
};
