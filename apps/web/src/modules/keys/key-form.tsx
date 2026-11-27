"use client";

import { standardSchemaResolver } from "@hookform/resolvers/standard-schema";
import { Controller, useForm, useWatch } from "react-hook-form";
import * as z from "zod";

import {
  KEY_EXPIRIES,
  KEY_EXPIRY_CHOICES,
  KEY_EXPIRY_SECONDS,
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

import {
  LimitFields,
  limitValuesSchema,
  toKeyLimit,
  toLimitValues,
} from "~/modules/keys/limit-fields";
import { useNow } from "~/modules/keys/use-now";

import type { KeyExpiryChoice } from "@workspace/api/schema";
import type { KeyCreationSettings } from "~/modules/keys/lib/api";
import type { KeyLimitInput } from "~/modules/keys/limit-fields";

export interface CreateKeyResult {
  name: string;
  expiry: KeyExpiryChoice;
  limit: KeyLimitInput | null;
}

interface CreateKeyModalProps {
  readonly open: boolean;
  readonly onOpenChange: (open: boolean) => void;
  readonly settings: KeyCreationSettings | undefined;
  readonly onSubmit: (result: CreateKeyResult) => Promise<unknown>;
}

/** The lifetimes the platform's maximum allows; "never" only without one (ADMIN-12). */
export const allowedExpiries = (
  maxLifetime: KeyCreationSettings["maxLifetime"],
): KeyExpiryChoice[] =>
  maxLifetime === null
    ? [...KEY_EXPIRY_CHOICES]
    : KEY_EXPIRIES.filter(
        (choice) =>
          KEY_EXPIRY_SECONDS[choice] <= KEY_EXPIRY_SECONDS[maxLifetime],
      );

/** "Nov 27, 2026, 3:04 AM GMT+3", in the reader's own time zone. */
export const formatExpiry = (date: Date, locale: string) =>
  new Intl.DateTimeFormat(locale, {
    dateStyle: "medium",
    timeStyle: "short",
    timeZoneName: "short",
  }).format(date);

/**
 * Name, expiration (required, fixed once the key exists: KEYS-03) and credit
 * limit with its reset, starting from the platform's defaults (ADMIN-12).
 */
export const CreateKeyModal = ({
  open,
  onOpenChange,
  settings,
  onSubmit,
}: CreateKeyModalProps) => {
  const { t, i18n } = useTranslation(["common", "organization"]);
  const expiries = allowedExpiries(settings?.maxLifetime ?? null);

  const schema = z.object({
    name: z.string().trim().min(1).max(100),
    expiry: z.enum(KEY_EXPIRY_CHOICES),
    limit: limitValuesSchema(t("organization:keys.form.limit.invalid")),
  });

  const form = useForm({
    resolver: standardSchemaResolver(schema),
    values: {
      name: "",
      expiry: settings?.defaultExpiry ?? "90d",
      limit: toLimitValues(
        settings?.defaultLimitMicroSar ?? null,
        settings?.defaultLimitReset ?? null,
        settings?.limitPresetsMicroSar ?? [],
      ),
    },
  });
  const expiry = useWatch({ control: form.control, name: "expiry" });

  // the preview moves with the clock while the form is open
  const now = useNow(15_000);
  const preview =
    expiry === "never"
      ? t("organization:keys.form.expiry.previewNever")
      : t("organization:keys.form.expiry.preview", {
          date: formatExpiry(
            new Date(now + KEY_EXPIRY_SECONDS[expiry] * 1000),
            i18n.language,
          ),
        });

  const submit = async (values: z.infer<typeof schema>) => {
    await onSubmit({
      name: values.name,
      expiry: values.expiry,
      limit: toKeyLimit(values.limit),
    });
    form.reset();
  };

  const expiryItems = expiries.map((value) => ({
    value,
    label: t(`organization:keys.form.expiry.${value}`),
  }));
  const isDisabled = form.formState.isSubmitting || !settings;

  return (
    <Modal open={open} drawer={{ onOpenChange }} dialog={{ onOpenChange }}>
      <ModalContent>
        <ModalHeader>
          <ModalTitle>{t("organization:keys.create.title")}</ModalTitle>
          <ModalDescription>
            {t("organization:keys.create.description")}
          </ModalDescription>
        </ModalHeader>
        <ModalBody>
          <form
            id="key-create-form"
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
                    disabled={isDisabled}
                  />
                  {fieldState.invalid && (
                    <FieldError errors={[fieldState.error]} />
                  )}
                </Field>
              )}
            />
            <Controller
              name="expiry"
              control={form.control}
              render={({ field }) => (
                <Field>
                  <FieldLabel htmlFor="key-expiry">
                    {t("organization:keys.form.expiry.label")}
                  </FieldLabel>
                  <Select
                    items={expiryItems}
                    value={field.value}
                    onValueChange={(value) => {
                      const choice = expiries.find((item) => item === value);
                      if (choice) field.onChange(choice);
                    }}
                    disabled={isDisabled}
                  >
                    <SelectTrigger id="key-expiry" className="w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {expiryItems.map((item) => (
                        <SelectItem key={item.value} value={item.value}>
                          {item.label}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <FieldDescription aria-live="polite">
                    {preview}
                  </FieldDescription>
                  {settings?.maxLifetime && (
                    <FieldDescription>
                      {t("organization:keys.form.expiry.maximum", {
                        lifetime: t(
                          `organization:keys.form.expiry.${settings.maxLifetime}`,
                        ),
                      })}
                    </FieldDescription>
                  )}
                </Field>
              )}
            />
            <Controller
              name="limit"
              control={form.control}
              render={({ field }) => (
                <LimitFields
                  idPrefix="key-create"
                  value={field.value}
                  onChange={field.onChange}
                  error={form.formState.errors.limit?.limitSar?.message}
                  presets={settings?.limitPresetsMicroSar ?? []}
                  disabled={isDisabled}
                />
              )}
            />
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
          <Button type="submit" form="key-create-form" disabled={isDisabled}>
            {form.formState.isSubmitting ? (
              <Spinner />
            ) : (
              t("organization:keys.create.cta")
            )}
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>
  );
};
