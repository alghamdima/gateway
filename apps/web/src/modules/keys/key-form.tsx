"use client";

import { standardSchemaResolver } from "@hookform/resolvers/standard-schema";
import { Controller, useForm, useWatch } from "react-hook-form";
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
import {
  Modal,
  ModalBody,
  ModalClose,
  ModalContent,
  ModalDescription,
  ModalFooter,
  ModalHeader,
  ModalTitle,
} from "@workspace/ui-web/modal";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@workspace/ui-web/select";
import { Spinner } from "@workspace/ui-web/spinner";

import { MICRO_USD, usdToMicroUsd } from "~/modules/keys/money";

export const RESETS = ["never", "daily", "weekly", "monthly"] as const;
export type Reset = (typeof RESETS)[number];

export interface KeyFormValues {
  name: string;
  /** Empty: no limit. */
  limitUsd: string;
  reset: Reset;
}

export interface KeyFormResult {
  name: string;
  limit: {
    limitMicroUsd: number;
    reset: Exclude<Reset, "never"> | null;
  } | null;
}

interface KeyFormModalProps {
  readonly open: boolean;
  readonly onOpenChange: (open: boolean) => void;
  readonly title: string;
  readonly submitLabel: string;
  readonly initial?: Partial<KeyFormValues>;
  readonly onSubmit: (result: KeyFormResult) => Promise<unknown>;
}

/** Name, and an optional spending cap with its reset: for creating and for editing a key. */
export const KeyFormModal = ({
  open,
  onOpenChange,
  title,
  submitLabel,
  initial,
  onSubmit,
}: KeyFormModalProps) => {
  const { t } = useTranslation(["common", "organization"]);

  const schema = z.object({
    name: z.string().trim().min(1).max(100),
    limitUsd: z
      .string()
      .trim()
      .refine(
        (value) =>
          value === "" ||
          (Number.isFinite(Number(value)) &&
            Number(value) > 0 &&
            Number(value) <= 1_000_000),
        { message: t("organization:keys.form.limit.invalid") },
      ),
    reset: z.enum(RESETS),
  });

  const form = useForm({
    resolver: standardSchemaResolver(schema),
    values: {
      name: initial?.name ?? "",
      limitUsd: initial?.limitUsd ?? "",
      reset: initial?.reset ?? "monthly",
    },
  });
  const limitUsd = useWatch({ control: form.control, name: "limitUsd" });
  const hasLimit = limitUsd.trim() !== "";

  const submit = async (values: z.infer<typeof schema>) => {
    await onSubmit({
      name: values.name,
      limit:
        values.limitUsd === ""
          ? null
          : {
              limitMicroUsd: usdToMicroUsd(Number(values.limitUsd)),
              reset: values.reset === "never" ? null : values.reset,
            },
    });
  };

  const resets = RESETS.map((value) => ({
    value,
    label: t(`organization:keys.form.reset.${value}`),
  }));

  return (
    <Modal open={open} drawer={{ onOpenChange }} dialog={{ onOpenChange }}>
      <ModalContent>
        <ModalHeader>
          <ModalTitle>{title}</ModalTitle>
          <ModalDescription>
            {t("organization:keys.form.description")}
          </ModalDescription>
        </ModalHeader>
        <ModalBody>
          <form
            id="key-form"
            noValidate
            onSubmit={form.handleSubmit(submit)}
            className="space-y-6"
          >
            <Controller
              name="name"
              control={form.control}
              render={({ field, fieldState }) => (
                <Field data-invalid={fieldState.invalid}>
                  <FieldLabel htmlFor="key-name">
                    {t("organization:keys.form.name.label")}
                  </FieldLabel>
                  <Input
                    id="key-name"
                    autoComplete="off"
                    placeholder={t("organization:keys.form.name.placeholder")}
                    {...field}
                    aria-invalid={fieldState.invalid}
                    disabled={form.formState.isSubmitting}
                  />
                  {fieldState.invalid && (
                    <FieldError errors={[fieldState.error]} />
                  )}
                </Field>
              )}
            />
            <div className="grid gap-4 sm:grid-cols-2">
              <Controller
                name="limitUsd"
                control={form.control}
                render={({ field, fieldState }) => (
                  <Field data-invalid={fieldState.invalid}>
                    <FieldLabel htmlFor="key-limit">
                      {t("organization:keys.form.limit.label")}
                    </FieldLabel>
                    <div className="flex items-center gap-2">
                      <span className="text-muted-foreground">$</span>
                      <Input
                        id="key-limit"
                        inputMode="decimal"
                        placeholder={t(
                          "organization:keys.form.limit.placeholder",
                        )}
                        {...field}
                        aria-invalid={fieldState.invalid}
                        disabled={form.formState.isSubmitting}
                      />
                    </div>
                    {fieldState.invalid && (
                      <FieldError errors={[fieldState.error]} />
                    )}
                  </Field>
                )}
              />
              <Controller
                name="reset"
                control={form.control}
                render={({ field }) => (
                  <Field>
                    <FieldLabel htmlFor="key-reset">
                      {t("organization:keys.form.reset.label")}
                    </FieldLabel>
                    <Select
                      items={resets}
                      value={field.value}
                      onValueChange={field.onChange}
                      disabled={!hasLimit || form.formState.isSubmitting}
                    >
                      <SelectTrigger id="key-reset" className="w-full">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {resets.map((reset) => (
                          <SelectItem key={reset.value} value={reset.value}>
                            {reset.label}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </Field>
                )}
              />
            </div>
            <FieldDescription>
              {t("organization:keys.form.limit.description")}
            </FieldDescription>
          </form>
        </ModalBody>
        <ModalFooter>
          <ModalClose
            render={
              <Button variant="outline" type="button">
                {t("cancel")}
              </Button>
            }
          />
          <Button
            type="submit"
            form="key-form"
            disabled={form.formState.isSubmitting}
          >
            {form.formState.isSubmitting ? <Spinner /> : submitLabel}
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>
  );
};

/** The form's values for a key as the API returns it. */
export const toFormValues = (key: {
  name: string;
  limitMicroUsd: number | null;
  limitReset: string | null;
}): KeyFormValues => ({
  name: key.name,
  limitUsd:
    key.limitMicroUsd === null ? "" : String(key.limitMicroUsd / MICRO_USD),
  reset:
    key.limitMicroUsd === null
      ? "monthly"
      : (RESETS.find((reset) => reset === key.limitReset) ?? "never"),
});
