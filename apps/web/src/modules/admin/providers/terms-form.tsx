"use client";

import { standardSchemaResolver } from "@hookform/resolvers/standard-schema";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Controller, useForm } from "react-hook-form";
import * as z from "zod";

import { setProviderTermsSchema } from "@workspace/api/schema";
import { useTranslation } from "@workspace/i18n";
import { Button } from "@workspace/ui-web/button";
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
} from "@workspace/ui-web/field";
import { Input } from "@workspace/ui-web/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@workspace/ui-web/select";
import { Spinner } from "@workspace/ui-web/spinner";
import { toast } from "@workspace/ui-web/toast";

import { admin } from "~/modules/admin/lib/api";

type Terms = z.infer<typeof setProviderTermsSchema>;

export interface CatalogEntry {
  keyId: string;
  name: string;
  provider: string;
  isEnabled: boolean;
  isInGateway: boolean;
  terms: (Terms & { updatedBy: string; updatedAt: string }) | null;
  isZeroRetention: boolean;
  isNeverTrainedOn: boolean;
}

const REGIONS = setProviderTermsSchema.shape.region.options;
const AGREEMENTS = setProviderTermsSchema.shape.agreement.options;
const TRAINING = ["unknown", "no", "yes"] as const;

const trainingOf = (
  trains: boolean | null | undefined,
): (typeof TRAINING)[number] => {
  if (trains === null || trains === undefined) {
    return "unknown";
  }
  return trains ? "yes" : "no";
};

const today = () => new Date().toISOString().slice(0, 10);

interface TermsFormProps {
  readonly entry: CatalogEntry;
  readonly onDone: () => void;
}

export const TermsForm = ({ entry, onDone }: TermsFormProps) => {
  const { t } = useTranslation(["common", "admin"]);
  const queryClient = useQueryClient();

  const schema = z.object({
    region: z.enum(REGIONS),
    // empty is unknown, and unknown counts as kept (ADR 0015)
    retentionDays: z
      .number()
      .int()
      .min(0)
      .max(36_500)
      .or(z.nan())
      .transform((value) => (Number.isNaN(value) ? null : value)),
    training: z.enum(TRAINING),
    agreement: z.enum(AGREEMENTS),
    source: setProviderTermsSchema.shape.source,
    verifiedOn: setProviderTermsSchema.shape.verifiedOn,
  });

  const form = useForm({
    resolver: standardSchemaResolver(schema),
    defaultValues: {
      region: entry.terms?.region ?? "other",
      retentionDays: entry.terms?.retentionDays ?? Number.NaN,
      training: trainingOf(entry.terms?.trainsOnData),
      agreement: entry.terms?.agreement ?? "public_terms",
      source: entry.terms?.source ?? "",
      verifiedOn: entry.terms?.verifiedOn ?? today(),
    },
  });

  const onChanged = async (title: string) => {
    await queryClient.invalidateQueries({ queryKey: ["admin", "providers"] });
    toast.add({ title, type: "success" });
    onDone();
  };

  const setTerms = useMutation({
    ...admin.mutations.providers.setTerms,
    onSuccess: () => onChanged(t("admin:providers.saved")),
  });

  const removeTerms = useMutation({
    ...admin.mutations.providers.removeTerms,
    onSuccess: () => onChanged(t("admin:providers.removed")),
  });

  const onSubmit = async ({ training, ...data }: z.output<typeof schema>) => {
    await setTerms.mutateAsync({
      keyId: entry.keyId,
      ...data,
      trainsOnData: training === "unknown" ? null : training === "yes",
    });
  };

  const isBusy = form.formState.isSubmitting || removeTerms.isPending;
  const regions = REGIONS.map((value) => ({
    value,
    label: t(`admin:providers.regions.${value}`),
  }));
  const agreements = AGREEMENTS.map((value) => ({
    value,
    label: t(`admin:providers.agreements.${value}`),
  }));
  const training = TRAINING.map((value) => ({
    value,
    label: t(`admin:providers.trainingOptions.${value}`),
  }));

  return (
    <form
      id="admin-provider-terms-form"
      noValidate
      onSubmit={form.handleSubmit(onSubmit)}
      className="space-y-4"
    >
      <div className="grid gap-4 @2xl:grid-cols-3">
        <Controller
          name="region"
          control={form.control}
          render={({ field }) => (
            <Field>
              <FieldLabel htmlFor="provider-region">
                {t("admin:providers.columns.region")}
              </FieldLabel>
              <Select
                items={regions}
                value={field.value}
                onValueChange={field.onChange}
                disabled={isBusy}
              >
                <SelectTrigger id="provider-region" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {regions.map((item) => (
                    <SelectItem key={item.value} value={item.value}>
                      {item.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </Field>
          )}
        />
        <Controller
          name="retentionDays"
          control={form.control}
          render={({ field, fieldState }) => (
            <Field data-invalid={fieldState.invalid}>
              <FieldLabel htmlFor="provider-retention">
                {t("admin:providers.form.retention.label")}
              </FieldLabel>
              <Input
                id="provider-retention"
                type="number"
                inputMode="numeric"
                step="1"
                min={0}
                value={Number.isNaN(field.value) ? "" : field.value}
                onChange={(event) => field.onChange(event.target.valueAsNumber)}
                onBlur={field.onBlur}
                aria-invalid={fieldState.invalid}
                aria-describedby="provider-retention-help"
                disabled={isBusy}
              />
              <FieldDescription id="provider-retention-help">
                {t("admin:providers.form.retention.description")}
              </FieldDescription>
              {fieldState.invalid && <FieldError errors={[fieldState.error]} />}
            </Field>
          )}
        />
        <Controller
          name="training"
          control={form.control}
          render={({ field }) => (
            <Field>
              <FieldLabel htmlFor="provider-training">
                {t("admin:providers.columns.training")}
              </FieldLabel>
              <Select
                items={training}
                value={field.value}
                onValueChange={field.onChange}
                disabled={isBusy}
              >
                <SelectTrigger id="provider-training" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {training.map((item) => (
                    <SelectItem key={item.value} value={item.value}>
                      {item.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </Field>
          )}
        />
        <Controller
          name="agreement"
          control={form.control}
          render={({ field }) => (
            <Field>
              <FieldLabel htmlFor="provider-agreement">
                {t("admin:providers.columns.agreement")}
              </FieldLabel>
              <Select
                items={agreements}
                value={field.value}
                onValueChange={field.onChange}
                disabled={isBusy}
              >
                <SelectTrigger id="provider-agreement" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {agreements.map((item) => (
                    <SelectItem key={item.value} value={item.value}>
                      {item.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </Field>
          )}
        />
        <Controller
          name="source"
          control={form.control}
          render={({ field, fieldState }) => (
            <Field data-invalid={fieldState.invalid}>
              <FieldLabel htmlFor="provider-source">
                {t("admin:providers.form.source.label")}
              </FieldLabel>
              <Input
                id="provider-source"
                autoComplete="off"
                placeholder={t("admin:providers.form.source.placeholder")}
                {...field}
                aria-invalid={fieldState.invalid}
                disabled={isBusy}
              />
              {fieldState.invalid && <FieldError errors={[fieldState.error]} />}
            </Field>
          )}
        />
        <Controller
          name="verifiedOn"
          control={form.control}
          render={({ field, fieldState }) => (
            <Field data-invalid={fieldState.invalid}>
              <FieldLabel htmlFor="provider-verified">
                {t("admin:providers.columns.verified")}
              </FieldLabel>
              <Input
                id="provider-verified"
                type="date"
                {...field}
                aria-invalid={fieldState.invalid}
                disabled={isBusy}
              />
              {fieldState.invalid && <FieldError errors={[fieldState.error]} />}
            </Field>
          )}
        />
      </div>
      <div className="flex flex-wrap gap-2">
        <Button type="submit" disabled={isBusy || !entry.isInGateway}>
          {form.formState.isSubmitting ? <Spinner /> : t("save")}
        </Button>
        <Button
          type="button"
          variant="ghost"
          disabled={isBusy}
          onClick={onDone}
        >
          {t("cancel")}
        </Button>
        {entry.terms && (
          <Button
            type="button"
            variant="outline"
            className="ml-auto"
            disabled={isBusy}
            onClick={() => removeTerms.mutate({ keyId: entry.keyId })}
          >
            {t("admin:providers.form.remove")}
          </Button>
        )}
      </div>
      {!entry.isInGateway && (
        <FieldDescription>{t("admin:providers.form.gone")}</FieldDescription>
      )}
    </form>
  );
};
