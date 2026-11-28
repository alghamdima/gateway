"use client";

import { standardSchemaResolver } from "@hookform/resolvers/standard-schema";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Controller, useForm, useWatch } from "react-hook-form";
import * as z from "zod";

import {
  MAX_CREDIT_EXPIRY_DAYS,
  MAX_REFUND_WINDOW_HOURS,
} from "@workspace/api/schema";
import { useTranslation } from "@workspace/i18n";
import { Button } from "@workspace/ui-web/button";
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
} from "@workspace/ui-web/field";
import { Input } from "@workspace/ui-web/input";
import { Label } from "@workspace/ui-web/label";
import { Spinner } from "@workspace/ui-web/spinner";
import { Switch } from "@workspace/ui-web/switch";
import { toast } from "@workspace/ui-web/toast";

import { admin } from "~/modules/admin/lib/api";

interface CreditTermsFormProps {
  readonly canEdit: boolean;
  readonly refundWindowHours: number;
  /** Null: credit never expires. */
  readonly creditExpiryDays: number | null;
}

/**
 * The self-serve refund window and the credit expiry (ADMIN-23). Each
 * purchase keeps the terms it was bought on; 0 hours also switches every
 * self-serve refund off at once.
 */
export const CreditTermsForm = ({
  canEdit,
  refundWindowHours,
  creditExpiryDays,
}: CreditTermsFormProps) => {
  const { t } = useTranslation(["common", "admin"]);
  const queryClient = useQueryClient();

  // the same bounds as the API and the database checks
  const whole = (min: number, max: number) =>
    z
      .number(t("admin:pricing.creditTerms.error.whole"))
      .int(t("admin:pricing.creditTerms.error.whole"))
      .min(min, t("admin:pricing.creditTerms.error.range", { min, max }))
      .max(max, t("admin:pricing.creditTerms.error.range", { min, max }));
  const schema = z
    .object({
      refundWindowHours: whole(0, MAX_REFUND_WINDOW_HOURS),
      expires: z.boolean(),
      creditExpiryDays: z.number().or(z.nan()),
    })
    .superRefine((value, ctx) => {
      if (value.expires) {
        const days = whole(1, MAX_CREDIT_EXPIRY_DAYS).safeParse(
          value.creditExpiryDays,
        );
        if (!days.success) {
          ctx.addIssue({
            code: "custom",
            path: ["creditExpiryDays"],
            message: days.error.issues[0]?.message,
          });
        }
      }
    });

  const form = useForm({
    resolver: standardSchemaResolver(schema),
    values: {
      refundWindowHours,
      expires: creditExpiryDays !== null,
      creditExpiryDays: creditExpiryDays ?? Number.NaN,
    },
  });
  const expires = useWatch({ control: form.control, name: "expires" });

  const update = useMutation({
    ...admin.mutations.pricing.updateCreditTerms,
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["admin", "pricing"] });
      toast.add({ title: t("admin:pricing.saved"), type: "success" });
    },
  });

  const onSubmit = async (data: z.infer<typeof schema>) => {
    await update.mutateAsync({
      refundWindowHours: data.refundWindowHours,
      creditExpiryDays: data.expires ? data.creditExpiryDays : null,
    });
  };

  const isDisabled = form.formState.isSubmitting || !canEdit;

  return (
    <form
      id="admin-pricing-credit-terms-form"
      noValidate
      onSubmit={form.handleSubmit(onSubmit)}
      className="space-y-6"
    >
      <div className="grid gap-6 @lg:grid-cols-2">
        <Controller
          name="refundWindowHours"
          control={form.control}
          render={({ field, fieldState }) => (
            <Field data-invalid={fieldState.invalid}>
              <FieldLabel htmlFor="admin-pricing-refund-window">
                {t("admin:pricing.creditTerms.refundWindow.label")}
              </FieldLabel>
              <div className="flex items-center gap-2">
                <Input
                  id="admin-pricing-refund-window"
                  type="number"
                  inputMode="numeric"
                  step="1"
                  min={0}
                  max={MAX_REFUND_WINDOW_HOURS}
                  value={Number.isNaN(field.value) ? "" : field.value}
                  onChange={(event) =>
                    field.onChange(event.target.valueAsNumber)
                  }
                  onBlur={field.onBlur}
                  disabled={isDisabled}
                  aria-invalid={fieldState.invalid}
                  className="max-w-32"
                />
                <span className="text-muted-foreground">
                  {t("admin:pricing.creditTerms.hours")}
                </span>
              </div>
              <FieldDescription>
                {t("admin:pricing.creditTerms.refundWindow.description")}
              </FieldDescription>
              {fieldState.invalid && <FieldError errors={[fieldState.error]} />}
            </Field>
          )}
        />
        <div className="space-y-3">
          <Controller
            name="expires"
            control={form.control}
            render={({ field }) => (
              <div className="flex items-center gap-2">
                <Switch
                  id="admin-pricing-credit-expires"
                  checked={field.value}
                  disabled={isDisabled}
                  onCheckedChange={field.onChange}
                />
                <Label htmlFor="admin-pricing-credit-expires">
                  {t("admin:pricing.creditTerms.expiry.switch")}
                </Label>
              </div>
            )}
          />
          <Controller
            name="creditExpiryDays"
            control={form.control}
            render={({ field, fieldState }) => (
              <Field data-invalid={fieldState.invalid}>
                <FieldLabel htmlFor="admin-pricing-credit-expiry">
                  {t("admin:pricing.creditTerms.expiry.label")}
                </FieldLabel>
                <div className="flex items-center gap-2">
                  <Input
                    id="admin-pricing-credit-expiry"
                    type="number"
                    inputMode="numeric"
                    step="1"
                    min={1}
                    max={MAX_CREDIT_EXPIRY_DAYS}
                    value={Number.isNaN(field.value) ? "" : field.value}
                    onChange={(event) =>
                      field.onChange(event.target.valueAsNumber)
                    }
                    onBlur={field.onBlur}
                    disabled={isDisabled || !expires}
                    aria-invalid={fieldState.invalid}
                    className="max-w-32"
                  />
                  <span className="text-muted-foreground">
                    {t("admin:pricing.creditTerms.days")}
                  </span>
                </div>
                <FieldDescription>
                  {t("admin:pricing.creditTerms.expiry.description")}
                </FieldDescription>
                {fieldState.invalid && (
                  <FieldError errors={[fieldState.error]} />
                )}
              </Field>
            )}
          />
        </div>
      </div>
      {canEdit && (
        <Button type="submit" disabled={isDisabled}>
          {form.formState.isSubmitting ? <Spinner /> : t("save")}
        </Button>
      )}
    </form>
  );
};
