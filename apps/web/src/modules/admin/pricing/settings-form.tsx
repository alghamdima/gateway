"use client";

import { standardSchemaResolver } from "@hookform/resolvers/standard-schema";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Controller, useForm } from "react-hook-form";
import * as z from "zod";

import { MAX_TOPUP_MICRO_USD, MAX_TOPUP_PRESETS } from "@workspace/api/schema";
import { useTranslation } from "@workspace/i18n";
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
  microUsdToUsd,
  percentSchema,
  percentToBps,
  usdSchema,
  usdToMicroUsd,
} from "~/modules/admin/pricing/rates";

interface SettingsFormProps {
  readonly canEdit: boolean;
  readonly defaultMarkupBps: number;
  readonly topupFeeBps: number;
  readonly topupFeeMinMicroUsd: number;
  readonly topupMinMicroUsd: number;
  readonly topupMaxMicroUsd: number;
  readonly topupPresetsMicroUsd: number[];
}

export const SettingsForm = ({
  canEdit,
  defaultMarkupBps,
  topupFeeBps,
  topupFeeMinMicroUsd,
  topupMinMicroUsd,
  topupMaxMicroUsd,
  topupPresetsMicroUsd,
}: SettingsFormProps) => {
  const { t } = useTranslation(["common", "admin"]);
  const queryClient = useQueryClient();

  const amount = usdSchema(
    microUsdToUsd(MAX_TOPUP_MICRO_USD),
    t("admin:pricing.error.decimals"),
  );
  // the same rules as the API and the database checks (ADMIN-05)
  const schema = z
    .object({
      defaultMarkup: percentSchema(1000, t("admin:pricing.error.decimals")),
      topupFee: percentSchema(100, t("admin:pricing.error.decimals")),
      // dollars and cents, at most $100: the same two-decimal rule as a percentage
      topupFeeMin: percentSchema(100, t("admin:pricing.error.decimals")),
      topupMin: amount,
      topupMax: amount,
      topupPresets: z.string(),
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
      topupFeeMin: microUsdToUsd(topupFeeMinMicroUsd),
      topupMin: microUsdToUsd(topupMinMicroUsd),
      topupMax: microUsdToUsd(topupMaxMicroUsd),
      topupPresets: topupPresetsMicroUsd
        .map((preset) => microUsdToUsd(preset))
        .join(", "),
    },
  });

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
      topupFeeMinMicroUsd: usdToMicroUsd(data.topupFeeMin),
      topupMinMicroUsd: usdToMicroUsd(data.topupMin),
      topupMaxMicroUsd: usdToMicroUsd(data.topupMax),
      topupPresetsMicroUsd: (parsePresets(data.topupPresets) ?? []).map(
        (preset) => usdToMicroUsd(preset),
      ),
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
                    <span className="text-muted-foreground">$</span>
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
                  <span className="text-muted-foreground">$</span>
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
                <span className="text-muted-foreground">$</span>
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

/** "10, 25, 50" to dollar amounts; null when a part is not a number. */
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
