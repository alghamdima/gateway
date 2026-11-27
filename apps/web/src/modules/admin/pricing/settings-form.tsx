"use client";

import { standardSchemaResolver } from "@hookform/resolvers/standard-schema";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Controller, useForm, useWatch } from "react-hook-form";
import * as z from "zod";

import {
  MAX_FX_SPREAD_BPS,
  MAX_TOPUP_MICRO_SAR,
  MAX_TOPUP_PRESETS,
  MAX_USD_TO_SAR_RATE_E6,
  MIN_USD_TO_SAR_RATE_E6,
} from "@workspace/api/schema";
import { useTranslation } from "@workspace/i18n";
import { effectiveUsdToSarRateE6, formatRateE6 } from "@workspace/shared/utils";
import { Button } from "@workspace/ui-web/button";
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
} from "@workspace/ui-web/field";
import { Input } from "@workspace/ui-web/input";
import { Spinner } from "@workspace/ui-web/spinner";
import { toast } from "@workspace/ui-web/toast";

import { admin } from "~/modules/admin/lib/api";
import {
  bpsToPercent,
  microSarToSar,
  percentSchema,
  percentToBps,
  rateE6ToRate,
  rateToRateE6,
  sarSchema,
  sarToMicroSar,
} from "~/modules/admin/pricing/rates";

interface SettingsFormProps {
  readonly canEdit: boolean;
  readonly defaultMarkupBps: number;
  readonly topupFeeBps: number;
  readonly topupFeeMinMicroSar: number;
  readonly topupMinMicroSar: number;
  readonly topupMaxMicroSar: number;
  readonly topupPresetsMicroSar: number[];
  readonly usdToSarRateE6: number;
  readonly fxSpreadBps: number;
}

export const SettingsForm = ({
  canEdit,
  defaultMarkupBps,
  topupFeeBps,
  topupFeeMinMicroSar,
  topupMinMicroSar,
  topupMaxMicroSar,
  topupPresetsMicroSar,
  usdToSarRateE6,
  fxSpreadBps,
}: SettingsFormProps) => {
  const { t } = useTranslation(["common", "admin"]);
  const queryClient = useQueryClient();

  const amount = sarSchema(
    microSarToSar(MAX_TOPUP_MICRO_SAR),
    t("admin:pricing.error.decimals"),
  );
  // the same rules as the API and the database checks (ADMIN-05)
  const schema = z
    .object({
      defaultMarkup: percentSchema(1000, t("admin:pricing.error.decimals")),
      topupFee: percentSchema(100, t("admin:pricing.error.decimals")),
      // riyals and halalas, at most SAR 400: the same two-decimal rule as a percentage
      topupFeeMin: percentSchema(400, t("admin:pricing.error.decimals")),
      topupMin: amount,
      topupMax: amount,
      topupPresets: z.string(),
      // the official rate, 3 to 5 with at most six decimals (ADMIN-06)
      usdToSarRate: z
        .number()
        .min(
          rateE6ToRate(MIN_USD_TO_SAR_RATE_E6),
          t("admin:pricing.error.rate"),
        )
        .max(
          rateE6ToRate(MAX_USD_TO_SAR_RATE_E6),
          t("admin:pricing.error.rate"),
        )
        .refine(
          (value) =>
            Math.abs(value * 1_000_000 - Math.round(value * 1_000_000)) < 1e-6,
          { message: t("admin:pricing.error.rateDecimals") },
        ),
      fxSpread: percentSchema(
        bpsToPercent(MAX_FX_SPREAD_BPS),
        t("admin:pricing.error.decimals"),
      ),
    })
    .superRefine((value, ctx) => {
      if (value.topupMax < value.topupMin) {
        ctx.addIssue({
          code: "custom",
          path: ["topupMax"],
          message: t("admin:pricing.error.maxBelowMin"),
        });
      }
      const presets = parsePresets(value.topupPresets);
      if (
        !presets ||
        presets.length > MAX_TOPUP_PRESETS ||
        presets.some((preset) => !amount.safeParse(preset).success)
      ) {
        ctx.addIssue({
          code: "custom",
          path: ["topupPresets"],
          message: t("admin:pricing.error.presets", {
            count: MAX_TOPUP_PRESETS,
          }),
        });
        return;
      }
      if (
        presets.some(
          (preset) => preset < value.topupMin || preset > value.topupMax,
        )
      ) {
        ctx.addIssue({
          code: "custom",
          path: ["topupPresets"],
          message: t("admin:pricing.error.presetOutOfRange"),
        });
      }
    });

  const form = useForm({
    resolver: standardSchemaResolver(schema),
    values: {
      defaultMarkup: bpsToPercent(defaultMarkupBps),
      topupFee: bpsToPercent(topupFeeBps),
      topupFeeMin: microSarToSar(topupFeeMinMicroSar),
      topupMin: microSarToSar(topupMinMicroSar),
      topupMax: microSarToSar(topupMaxMicroSar),
      topupPresets: topupPresetsMicroSar
        .map((preset) => microSarToSar(preset))
        .join(", "),
      usdToSarRate: rateE6ToRate(usdToSarRateE6),
      fxSpread: bpsToPercent(fxSpreadBps),
    },
  });

  // what new requests will convert at, worked out as the gateway does
  const [rate, spread] = useWatch({
    control: form.control,
    name: ["usdToSarRate", "fxSpread"],
  });
  const effectiveRate = toEffectiveRate(rate, spread);

  const updateSettings = useMutation({
    ...admin.mutations.pricing.updateSettings,
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["admin", "pricing"] });
      toast.add({ title: t("admin:pricing.saved"), type: "success" });
    },
  });

  const onSubmit = async (data: z.infer<typeof schema>) => {
    await updateSettings.mutateAsync({
      defaultMarkupBps: percentToBps(data.defaultMarkup),
      topupFeeBps: percentToBps(data.topupFee),
      topupFeeMinMicroSar: sarToMicroSar(data.topupFeeMin),
      topupMinMicroSar: sarToMicroSar(data.topupMin),
      topupMaxMicroSar: sarToMicroSar(data.topupMax),
      topupPresetsMicroSar: (parsePresets(data.topupPresets) ?? []).map(
        (preset) => sarToMicroSar(preset),
      ),
      usdToSarRateE6: rateToRateE6(data.usdToSarRate),
      fxSpreadBps: percentToBps(data.fxSpread),
    });
  };

  const fields = [
    {
      name: "defaultMarkup",
      label: t("admin:pricing.settings.defaultMarkup.label"),
      description: t("admin:pricing.settings.defaultMarkup.description"),
    },
    {
      name: "topupFee",
      label: t("admin:pricing.settings.topupFee.label"),
      description: t("admin:pricing.settings.topupFee.description"),
    },
    {
      name: "topupFeeMin",
      label: t("admin:pricing.settings.topupFeeMin.label"),
      description: t("admin:pricing.settings.topupFeeMin.description"),
    },
  ] as const;

  const limitFields = [
    {
      name: "topupMin",
      label: t("admin:pricing.settings.topupMin.label"),
      description: t("admin:pricing.settings.topupMin.description"),
    },
    {
      name: "topupMax",
      label: t("admin:pricing.settings.topupMax.label"),
      description: t("admin:pricing.settings.topupMax.description"),
    },
  ] as const;

  const isDisabled = form.formState.isSubmitting || !canEdit;

  return (
    <form
      id="admin-pricing-settings-form"
      noValidate
      onSubmit={form.handleSubmit(onSubmit)}
      className="space-y-6"
    >
      <div className="grid gap-6 @lg:grid-cols-3">
        {fields.map(({ name, label, description }) => (
          <Controller
            key={name}
            name={name}
            control={form.control}
            render={({ field, fieldState }) => (
              <Field data-invalid={fieldState.invalid}>
                <FieldLabel htmlFor={`admin-pricing-${name}`}>
                  {label}
                </FieldLabel>
                <div className="flex items-center gap-2">
                  {name === "topupFeeMin" && (
                    <span className="text-muted-foreground">SAR</span>
                  )}
                  <Input
                    id={`admin-pricing-${name}`}
                    type="number"
                    inputMode="decimal"
                    step="0.01"
                    min={0}
                    value={Number.isNaN(field.value) ? "" : field.value}
                    onChange={(event) =>
                      field.onChange(event.target.valueAsNumber)
                    }
                    onBlur={field.onBlur}
                    disabled={isDisabled}
                    aria-invalid={fieldState.invalid}
                    className="max-w-32"
                  />
                  {name !== "topupFeeMin" && (
                    <span className="text-muted-foreground">%</span>
                  )}
                </div>
                <FieldDescription>{description}</FieldDescription>
                {fieldState.invalid && (
                  <FieldError errors={[fieldState.error]} />
                )}
              </Field>
            )}
          />
        ))}
      </div>
      <div className="grid gap-6 @lg:grid-cols-3">
        {limitFields.map(({ name, label, description }) => (
          <Controller
            key={name}
            name={name}
            control={form.control}
            render={({ field, fieldState }) => (
              <Field data-invalid={fieldState.invalid}>
                <FieldLabel htmlFor={`admin-pricing-${name}`}>
                  {label}
                </FieldLabel>
                <div className="flex items-center gap-2">
                  <span className="text-muted-foreground">SAR</span>
                  <Input
                    id={`admin-pricing-${name}`}
                    type="number"
                    inputMode="decimal"
                    step="0.01"
                    min={0}
                    value={Number.isNaN(field.value) ? "" : field.value}
                    onChange={(event) =>
                      field.onChange(event.target.valueAsNumber)
                    }
                    onBlur={field.onBlur}
                    disabled={isDisabled}
                    aria-invalid={fieldState.invalid}
                    className="max-w-32"
                  />
                </div>
                <FieldDescription>{description}</FieldDescription>
                {fieldState.invalid && (
                  <FieldError errors={[fieldState.error]} />
                )}
              </Field>
            )}
          />
        ))}
        <Controller
          name="topupPresets"
          control={form.control}
          render={({ field, fieldState }) => (
            <Field data-invalid={fieldState.invalid}>
              <FieldLabel htmlFor="admin-pricing-topupPresets">
                {t("admin:pricing.settings.topupPresets.label")}
              </FieldLabel>
              <div className="flex items-center gap-2">
                <span className="text-muted-foreground">SAR</span>
                <Input
                  id="admin-pricing-topupPresets"
                  inputMode="decimal"
                  {...field}
                  disabled={isDisabled}
                  aria-invalid={fieldState.invalid}
                />
              </div>
              <FieldDescription>
                {t("admin:pricing.settings.topupPresets.description", {
                  count: MAX_TOPUP_PRESETS,
                })}
              </FieldDescription>
              {fieldState.invalid && <FieldError errors={[fieldState.error]} />}
            </Field>
          )}
        />
      </div>
      <div className="grid gap-6 @lg:grid-cols-3">
        <Controller
          name="usdToSarRate"
          control={form.control}
          render={({ field, fieldState }) => (
            <Field data-invalid={fieldState.invalid}>
              <FieldLabel htmlFor="admin-pricing-usdToSarRate">
                {t("admin:pricing.settings.usdToSarRate.label")}
              </FieldLabel>
              <div className="flex items-center gap-2">
                <span className="text-muted-foreground">
                  {t("admin:pricing.settings.usdToSarRate.unit")}
                </span>
                <Input
                  id="admin-pricing-usdToSarRate"
                  type="number"
                  inputMode="decimal"
                  step="0.000001"
                  min={rateE6ToRate(MIN_USD_TO_SAR_RATE_E6)}
                  max={rateE6ToRate(MAX_USD_TO_SAR_RATE_E6)}
                  value={Number.isNaN(field.value) ? "" : field.value}
                  onChange={(event) =>
                    field.onChange(event.target.valueAsNumber)
                  }
                  onBlur={field.onBlur}
                  disabled={isDisabled}
                  aria-invalid={fieldState.invalid}
                  className="max-w-32"
                />
                <span className="text-muted-foreground">SAR</span>
              </div>
              <FieldDescription>
                {t("admin:pricing.settings.usdToSarRate.description")}
              </FieldDescription>
              {fieldState.invalid && <FieldError errors={[fieldState.error]} />}
            </Field>
          )}
        />
        <Controller
          name="fxSpread"
          control={form.control}
          render={({ field, fieldState }) => (
            <Field data-invalid={fieldState.invalid}>
              <FieldLabel htmlFor="admin-pricing-fxSpread">
                {t("admin:pricing.settings.fxSpread.label")}
              </FieldLabel>
              <div className="flex items-center gap-2">
                <Input
                  id="admin-pricing-fxSpread"
                  type="number"
                  inputMode="decimal"
                  step="0.01"
                  min={0}
                  max={bpsToPercent(MAX_FX_SPREAD_BPS)}
                  value={Number.isNaN(field.value) ? "" : field.value}
                  onChange={(event) =>
                    field.onChange(event.target.valueAsNumber)
                  }
                  onBlur={field.onBlur}
                  disabled={isDisabled}
                  aria-invalid={fieldState.invalid}
                  className="max-w-32"
                />
                <span className="text-muted-foreground">%</span>
              </div>
              <FieldDescription>
                {t("admin:pricing.settings.fxSpread.description")}
              </FieldDescription>
              {fieldState.invalid && <FieldError errors={[fieldState.error]} />}
            </Field>
          )}
        />
        <Field>
          <FieldLabel>
            {t("admin:pricing.settings.effectiveRate.label")}
          </FieldLabel>
          <p
            className="text-lg font-medium tabular-nums"
            aria-live="polite"
            data-testid="effective-usd-to-sar-rate"
          >
            {effectiveRate === null
              ? "—"
              : t("admin:pricing.settings.effectiveRate.value", {
                  rate: formatRateE6(effectiveRate),
                })}
          </p>
          <FieldDescription>
            {t("admin:pricing.settings.effectiveRate.description")}
          </FieldDescription>
        </Field>
      </div>
      {canEdit && (
        <Button
          type="submit"
          disabled={form.formState.isSubmitting || !form.formState.isDirty}
        >
          {form.formState.isSubmitting ? <Spinner /> : t("save")}
        </Button>
      )}
    </form>
  );
};

/**
 * The official rate plus the spread, rounded up to the millionth as the
 * gateway does; null while either field does not hold a valid value.
 */
const toEffectiveRate = (rate: number, spread: number) => {
  const rateE6 = rateToRateE6(rate);
  const spreadBps = percentToBps(spread);
  if (
    !Number.isFinite(rate) ||
    !Number.isFinite(spread) ||
    rateE6 < MIN_USD_TO_SAR_RATE_E6 ||
    rateE6 > MAX_USD_TO_SAR_RATE_E6 ||
    spreadBps < 0 ||
    spreadBps > MAX_FX_SPREAD_BPS
  ) {
    return null;
  }
  return effectiveUsdToSarRateE6(rateE6, spreadBps);
};

/** "40, 100, 200" to riyal amounts; null when a part is not a number. */
const parsePresets = (value: string) => {
  const parts = value
    .split(",")
    .map((part) => part.trim())
    .filter((part) => part !== "");
  if (parts.length === 0 || parts.some((part) => !/^\d+(\.\d+)?$/.test(part))) {
    return null;
  }
  return parts.map(Number);
};
