"use client";

import { standardSchemaResolver } from "@hookform/resolvers/standard-schema";
import { Controller, useForm } from "react-hook-form";
import * as z from "zod";

import { MAX_OWNED_ORGANIZATIONS_CEILING } from "@workspace/api/schema";
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

interface OrganizationLimitFormProps {
  readonly id: string;
  /** The value in force; null: none of its own (a user following the general limit). */
  readonly value: number | null;
  /** Whether the field may be left empty, which saves null. */
  readonly isOptional?: boolean;
  readonly description: string;
  /** The field's label, the largest value it takes and what a wrong value is told; default: the organization limit. */
  readonly label?: string;
  readonly max?: number;
  readonly invalidMessage?: string;
  readonly onSave: (value: number | null) => Promise<unknown>;
}

/** One whole number staff set about accounts (ADR 0020): by default, how many organizations may be owned. */
export const OrganizationLimitForm = ({
  id,
  value,
  isOptional = false,
  description,
  label,
  max = MAX_OWNED_ORGANIZATIONS_CEILING,
  invalidMessage,
  onSave,
}: OrganizationLimitFormProps) => {
  const { t } = useTranslation(["common", "admin"]);

  const schema = z.object({
    limit: z
      .string()
      .trim()
      .refine(
        (text) =>
          (isOptional && text === "") ||
          (/^\d+$/.test(text) && Number(text) <= max),
        {
          message: invalidMessage ?? t("admin:accountSettings.limit.invalid"),
        },
      ),
  });
  const form = useForm({
    resolver: standardSchemaResolver(schema),
    values: { limit: value?.toString() ?? "" },
  });

  return (
    <form
      id={id}
      noValidate
      onSubmit={form.handleSubmit(({ limit }) =>
        onSave(limit === "" ? null : Number(limit)),
      )}
      className="flex flex-wrap items-start gap-4"
    >
      <Controller
        name="limit"
        control={form.control}
        render={({ field, fieldState }) => (
          <Field data-invalid={fieldState.invalid} className="w-56">
            <FieldLabel htmlFor={`${id}-limit`}>
              {label ?? t("admin:accountSettings.limit.label")}
            </FieldLabel>
            <Input
              id={`${id}-limit`}
              inputMode="numeric"
              placeholder={
                isOptional ? t("admin:accountSettings.limit.general") : ""
              }
              {...field}
              aria-invalid={fieldState.invalid}
              disabled={form.formState.isSubmitting}
            />
            {fieldState.invalid && <FieldError errors={[fieldState.error]} />}
          </Field>
        )}
      />
      <Button
        type="submit"
        className="mt-6"
        disabled={form.formState.isSubmitting}
      >
        {form.formState.isSubmitting ? <Spinner /> : t("common:save")}
      </Button>
      <FieldDescription className="w-full">{description}</FieldDescription>
    </form>
  );
};
