"use client";

import { standardSchemaResolver } from "@hookform/resolvers/standard-schema";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Controller, useForm } from "react-hook-form";
import * as z from "zod";

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
  usdToMicroUsd,
} from "~/modules/admin/pricing/rates";

interface SettingsFormProps {
  readonly canEdit: boolean;
  readonly defaultMarkupBps: number;
  readonly topupFeeBps: number;
  readonly topupFeeMinMicroUsd: number;
}

export const SettingsForm = ({
  canEdit,
  defaultMarkupBps,
  topupFeeBps,
  topupFeeMinMicroUsd,
}: SettingsFormProps) => {
  const { t } = useTranslation(["common", "admin"]);
  const queryClient = useQueryClient();

  const schema = z.object({
    defaultMarkup: percentSchema(1000, t("admin:pricing.error.decimals")),
    topupFee: percentSchema(100, t("admin:pricing.error.decimals")),
    // dollars and cents, at most $100: the same two-decimal rule as a percentage
    topupFeeMin: percentSchema(100, t("admin:pricing.error.decimals")),
  });

  const form = useForm({
    resolver: standardSchemaResolver(schema),
    values: {
      defaultMarkup: bpsToPercent(defaultMarkupBps),
      topupFee: bpsToPercent(topupFeeBps),
      topupFeeMin: microUsdToUsd(topupFeeMinMicroUsd),
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
                    disabled={form.formState.isSubmitting || !canEdit}
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
