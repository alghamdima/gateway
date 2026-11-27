"use client";

import { standardSchemaResolver } from "@hookform/resolvers/standard-schema";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Controller, useForm } from "react-hook-form";
import * as z from "zod";

import { handle } from "@workspace/api/utils";
import { useTranslation } from "@workspace/i18n";
import { formatMicroSar } from "@workspace/shared/utils";
import { Badge } from "@workspace/ui-web/badge";
import { Button } from "@workspace/ui-web/button";
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
} from "@workspace/ui-web/field";
import { Input } from "@workspace/ui-web/input";
import { Skeleton } from "@workspace/ui-web/skeleton";
import { Spinner } from "@workspace/ui-web/spinner";
import { Textarea } from "@workspace/ui-web/textarea";
import { toast } from "@workspace/ui-web/toast";

import { api } from "~/lib/api/client";
import { useStaffPermission } from "~/modules/admin/lib/use-staff-permission";
import {
  bpsToPercent,
  microSarToSar,
  percentToBps,
  sarToMicroSar,
} from "~/modules/admin/pricing/rates";

/** The API's bounds (MAX_TOPUP_FEE_BPS, MAX_TOPUP_FEE_MIN_MICRO_SAR), as staff type them. */
const MAX_PERCENT = 100;
const MAX_MINIMUM_SAR = 400;
const MAX_NOTE_LENGTH = 500;

const key = <Name extends string>(name: Name) =>
  `admin:organizations.organization.topupFee.${name}` as const;

interface OrganizationTopupFeeProps {
  readonly organizationId: string;
}

/**
 * The organization's own top-up fee (ADMIN-24): higher or lower than the
 * default, until an optional date. The API decides which fee a top-up pays
 * (packages/api/src/modules/pricing/fee.ts); this card only shows and edits it.
 */
export const OrganizationTopupFee = ({
  organizationId,
}: OrganizationTopupFeeProps) => {
  const { t, i18n } = useTranslation(["common", "admin"]);
  const queryClient = useQueryClient();
  const canUpdate = useStaffPermission({ pricing: ["update"] });
  const queryKey = ["admin", "organizations", organizationId, "top-up-fee"];

  const current = useQuery({
    queryKey,
    queryFn: () =>
      handle(api.admin.organizations[":id"]["top-up-fee"].$get)({
        param: { id: organizationId },
      }),
  });

  const schema = z
    .object({
      percent: z
        .string()
        .trim()
        .refine((value) => value === "" || isAmount(value, MAX_PERCENT), {
          message: t(key("invalidPercent")),
        }),
      minimum: z
        .string()
        .trim()
        .refine((value) => value === "" || isAmount(value, MAX_MINIMUM_SAR), {
          message: t(key("invalidMinimum")),
        }),
      validThrough: z
        .string()
        .refine((value) => value === "" || endOfDay(value) > new Date(), {
          message: t(key("invalidDate")),
        }),
      note: z.string().trim().max(MAX_NOTE_LENGTH),
    })
    .refine((value) => value.percent !== "" || value.minimum !== "", {
      message: t(key("nothingSet")),
      path: ["percent"],
    });

  const override = current.data?.override ?? null;
  const form = useForm({
    resolver: standardSchemaResolver(schema),
    values: {
      percent:
        override === null || override.feeBps === null
          ? ""
          : String(bpsToPercent(override.feeBps)),
      minimum:
        override === null || override.feeMinMicroSar === null
          ? ""
          : String(microSarToSar(override.feeMinMicroSar)),
      validThrough: override?.expiresAt ? toDateInput(override.expiresAt) : "",
      note: override?.note ?? "",
    },
  });

  const refresh = () => queryClient.invalidateQueries({ queryKey });

  const save = useMutation({
    mutationFn: (values: z.infer<typeof schema>) =>
      handle(api.admin.organizations[":id"]["top-up-fee"].$put)({
        param: { id: organizationId },
        json: {
          feeBps:
            values.percent === "" ? null : percentToBps(Number(values.percent)),
          feeMinMicroSar:
            values.minimum === ""
              ? null
              : sarToMicroSar(Number(values.minimum)),
          expiresAt:
            values.validThrough === ""
              ? null
              : endOfDay(values.validThrough).toISOString(),
          note: values.note,
        },
      }),
    onSuccess: async () => {
      await refresh();
      toast.add({ title: t(key("saved")), type: "success" });
    },
  });

  const clear = useMutation({
    mutationFn: () =>
      handle(api.admin.organizations[":id"]["top-up-fee"].$delete)({
        param: { id: organizationId },
      }),
    onSuccess: async () => {
      await refresh();
      toast.add({ title: t(key("cleared")), type: "success" });
    },
  });

  if (current.isPending || !current.data) {
    return <Skeleton className="h-14 w-full" />;
  }

  const { default: defaults, effective } = current.data;
  const rate = (bps: number, minimumMicroSar: number) =>
    t(key("rate"), {
      percent: new Intl.NumberFormat(i18n.language, {
        maximumFractionDigits: 2,
      }).format(bpsToPercent(bps)),
      minimum: formatMicroSar(minimumMicroSar, i18n.language),
    });
  const date = (value: string) =>
    new Intl.DateTimeFormat(i18n.language, { dateStyle: "medium" }).format(
      // the expiry is the start of the day after the last valid one
      new Date(new Date(value).getTime() - 1),
    );
  // the API resolves the fee by time: a fee of its own that no longer applies has expired
  const isExpired = override !== null && effective.source === "default";
  const isBusy = form.formState.isSubmitting || clear.isPending;

  return (
    <div className="flex flex-col gap-6">
      <dl className="grid gap-4 sm:grid-cols-3">
        <div>
          <dt className="text-muted-foreground text-sm">{t(key("default"))}</dt>
          <dd className="font-semibold tabular-nums">
            {rate(defaults.topupFeeBps, defaults.topupFeeMinMicroSar)}
          </dd>
        </div>
        <div>
          <dt className="text-muted-foreground text-sm">{t(key("current"))}</dt>
          <dd className="flex flex-col gap-1">
            {override ? (
              <>
                <span className="font-semibold tabular-nums">
                  {override.feeBps === null
                    ? t(key("fromDefault"))
                    : `${bpsToPercent(override.feeBps)}%`}
                  {" / "}
                  {override.feeMinMicroSar === null
                    ? t(key("fromDefault"))
                    : formatMicroSar(override.feeMinMicroSar, i18n.language)}
                </span>
                <span className="text-muted-foreground text-sm">
                  {override.expiresAt === null
                    ? t(key("noExpiry"))
                    : t(key(isExpired ? "expired" : "expires"), {
                        date: date(override.expiresAt),
                      })}
                </span>
                {override.note && (
                  <span className="text-muted-foreground text-sm">
                    {override.note}
                  </span>
                )}
              </>
            ) : (
              <span className="font-semibold">{t(key("none"))}</span>
            )}
          </dd>
        </div>
        <div>
          <dt className="text-muted-foreground text-sm">
            {t(key("effective"))}
          </dt>
          <dd className="flex flex-wrap items-center gap-2">
            <span className="font-semibold tabular-nums">
              {rate(effective.topupFeeBps, effective.topupFeeMinMicroSar)}
            </span>
            <Badge
              variant={
                effective.source === "organization" ? "default" : "secondary"
              }
            >
              {t(key(`source.${effective.source}`))}
            </Badge>
          </dd>
        </div>
      </dl>

      {canUpdate && (
        <form
          id="admin-topup-fee-form"
          noValidate
          onSubmit={form.handleSubmit((values) => save.mutateAsync(values))}
          className="flex flex-col gap-4"
        >
          <div className="flex flex-wrap items-start gap-4">
            {(
              [
                ["percent", "decimal"],
                ["minimum", "decimal"],
              ] as const
            ).map(([name, inputMode]) => (
              <Controller
                key={name}
                name={name}
                control={form.control}
                render={({ field, fieldState }) => (
                  <Field data-invalid={fieldState.invalid} className="w-40">
                    <FieldLabel htmlFor={`admin-topup-fee-${name}`}>
                      {t(key(name))}
                    </FieldLabel>
                    <Input
                      id={`admin-topup-fee-${name}`}
                      inputMode={inputMode}
                      placeholder={t(key("useDefault"))}
                      {...field}
                      aria-invalid={fieldState.invalid}
                      disabled={isBusy}
                    />
                    {fieldState.invalid && (
                      <FieldError errors={[fieldState.error]} />
                    )}
                  </Field>
                )}
              />
            ))}
            <Controller
              name="validThrough"
              control={form.control}
              render={({ field, fieldState }) => (
                <Field data-invalid={fieldState.invalid} className="w-48">
                  <FieldLabel htmlFor="admin-topup-fee-valid-through">
                    {t(key("validThrough"))}
                  </FieldLabel>
                  <Input
                    id="admin-topup-fee-valid-through"
                    type="date"
                    {...field}
                    aria-invalid={fieldState.invalid}
                    disabled={isBusy}
                  />
                  {fieldState.invalid && (
                    <FieldError errors={[fieldState.error]} />
                  )}
                </Field>
              )}
            />
          </div>
          <Controller
            name="note"
            control={form.control}
            render={({ field, fieldState }) => (
              <Field data-invalid={fieldState.invalid} className="max-w-xl">
                <FieldLabel htmlFor="admin-topup-fee-note">
                  {t(key("note"))}
                </FieldLabel>
                <Textarea
                  id="admin-topup-fee-note"
                  placeholder={t(key("notePlaceholder"))}
                  maxLength={MAX_NOTE_LENGTH}
                  {...field}
                  aria-invalid={fieldState.invalid}
                  disabled={isBusy}
                />
                {fieldState.invalid && (
                  <FieldError errors={[fieldState.error]} />
                )}
              </Field>
            )}
          />
          <div className="flex flex-wrap gap-2">
            <Button type="submit" disabled={isBusy}>
              {form.formState.isSubmitting ? <Spinner /> : t("common:save")}
            </Button>
            {override && (
              <Button
                type="button"
                variant="outline"
                disabled={isBusy}
                onClick={() => clear.mutate()}
              >
                {clear.isPending ? <Spinner /> : t(key("clear"))}
              </Button>
            )}
          </div>
          <FieldDescription>{t(key("description"))}</FieldDescription>
        </form>
      )}
    </div>
  );
};

/** A number from 0 to `max` with at most two decimals: whole basis points or halalas. */
const isAmount = (value: string, max: number) =>
  /^\d+(\.\d{1,2})?$/.test(value) && Number(value) <= max;

/** The start of the day after `yyyy-mm-dd`, in the staff member's time zone. */
const endOfDay = (value: string) => {
  const [year, month, day] = value.split("-").map(Number);
  return new Date(year ?? 0, (month ?? 1) - 1, (day ?? 1) + 1);
};

const pad = (value: number) => String(value).padStart(2, "0");

/** The last valid day of an expiry, as the date input shows it. */
const toDateInput = (expiresAt: string) => {
  const last = new Date(new Date(expiresAt).getTime() - 1);
  return `${last.getFullYear()}-${pad(last.getMonth() + 1)}-${pad(last.getDate())}`;
};
