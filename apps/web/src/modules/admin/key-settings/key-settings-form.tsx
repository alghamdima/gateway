"use client";

import { standardSchemaResolver } from "@hookform/resolvers/standard-schema";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Controller, useForm, useWatch } from "react-hook-form";
import * as z from "zod";

import {
  API_KEY_LIMIT_RESETS,
  KEY_EXPIRIES,
  KEY_EXPIRY_CHOICES,
  KEY_EXPIRY_SECONDS,
  MAX_KEY_LIMIT_MICRO_SAR,
  MAX_KEY_LIMIT_PRESETS,
} from "@workspace/api/schema";
import { useTranslation } from "@workspace/i18n";
import { MICRO_SAR, sarToMicroSar } from "@workspace/shared/utils";
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

import type { ApiKeyLimitReset, KeyExpiry } from "@workspace/api/schema";

interface KeySettingsFormProps {
  readonly canEdit: boolean;
  readonly settings: {
    defaultLimitMicroSar: number | null;
    defaultLimitReset: ApiKeyLimitReset | null;
    defaultExpiry: (typeof KEY_EXPIRY_CHOICES)[number];
    maxLifetime: KeyExpiry | null;
    limitPresetsMicroSar: number[];
  };
}

const RESET_CHOICES = ["never", ...API_KEY_LIMIT_RESETS] as const;
const LIFETIME_CHOICES = ["none", ...KEY_EXPIRIES] as const;
const MAX_LIMIT_SAR = MAX_KEY_LIMIT_MICRO_SAR / MICRO_SAR;

/** Riyals with at most two decimals, above zero, under the typo ceiling. */
const isAmount = (value: string) =>
  /^\d+(\.\d{1,2})?$/.test(value) &&
  Number(value) > 0 &&
  Number(value) <= MAX_LIMIT_SAR;

/** "50, 200, 1000" to riyal amounts; null when a part is not an amount. */
const parsePresets = (value: string) => {
  const parts = value
    .split(",")
    .map((part) => part.trim())
    .filter((part) => part !== "");
  if (
    parts.length === 0 ||
    parts.length > MAX_KEY_LIMIT_PRESETS ||
    !parts.every(isAmount)
  ) {
    return null;
  }
  return parts.map(Number);
};

/**
 * The key defaults (ADMIN-12): the default limit, reset and expiry the create
 * form starts from, the limit quick picks, and the maximum key lifetime. The
 * same rules as the API and the database checks on key_settings.
 */
export const KeySettingsForm = ({
  canEdit,
  settings,
}: KeySettingsFormProps) => {
  const { t } = useTranslation(["common", "admin", "organization"]);
  const queryClient = useQueryClient();

  const schema = z
    .object({
      defaultLimitSar: z.string().trim(),
      defaultLimitReset: z.enum(RESET_CHOICES),
      defaultExpiry: z.enum(KEY_EXPIRY_CHOICES),
      maxLifetime: z.enum(LIFETIME_CHOICES),
      presets: z.string(),
    })
    .superRefine((value, ctx) => {
      if (value.defaultLimitSar !== "" && !isAmount(value.defaultLimitSar)) {
        ctx.addIssue({
          code: "custom",
          path: ["defaultLimitSar"],
          message: t("admin:keySettings.form.defaultLimit.invalid"),
        });
      }
      if (!parsePresets(value.presets)) {
        ctx.addIssue({
          code: "custom",
          path: ["presets"],
          message: t("admin:keySettings.form.presets.invalid", {
            count: MAX_KEY_LIMIT_PRESETS,
          }),
        });
      }
      if (
        value.maxLifetime !== "none" &&
        (value.defaultExpiry === "never" ||
          KEY_EXPIRY_SECONDS[value.defaultExpiry] >
            KEY_EXPIRY_SECONDS[value.maxLifetime])
      ) {
        ctx.addIssue({
          code: "custom",
          path: ["defaultExpiry"],
          message: t("admin:keySettings.form.defaultBeyondMax"),
        });
      }
    });

  const initial: z.input<typeof schema> = {
    defaultLimitSar:
      settings.defaultLimitMicroSar === null
        ? ""
        : String(settings.defaultLimitMicroSar / MICRO_SAR),
    defaultLimitReset: settings.defaultLimitReset ?? "never",
    defaultExpiry: settings.defaultExpiry,
    maxLifetime: settings.maxLifetime ?? "none",
    presets: settings.limitPresetsMicroSar
      .map((preset) => String(preset / MICRO_SAR))
      .join(", "),
  };
  const form = useForm({
    resolver: standardSchemaResolver(schema),
    values: initial,
  });
  const defaultLimitSar = useWatch({
    control: form.control,
    name: "defaultLimitSar",
  });

  const update = useMutation({
    ...admin.mutations.keySettings.update,
    onSuccess: async ({ lifetime }) => {
      await queryClient.invalidateQueries({
        queryKey: ["admin", "key-settings"],
      });
      toast.add({
        title:
          lifetime.shortened > 0
            ? t("admin:keySettings.savedShortened", {
                count: lifetime.shortened,
              })
            : t("admin:keySettings.saved"),
        type: "success",
      });
    },
  });

  const onSubmit = async (data: z.infer<typeof schema>) => {
    const limit =
      data.defaultLimitSar === ""
        ? null
        : sarToMicroSar(Number(data.defaultLimitSar));
    await update.mutateAsync({
      defaultLimitMicroSar: limit,
      defaultLimitReset:
        limit === null || data.defaultLimitReset === "never"
          ? null
          : data.defaultLimitReset,
      defaultExpiry: data.defaultExpiry,
      maxLifetime: data.maxLifetime === "none" ? null : data.maxLifetime,
      limitPresetsMicroSar: (parsePresets(data.presets) ?? []).map(
        sarToMicroSar,
      ),
    });
  };

  const isDisabled = form.formState.isSubmitting || !canEdit;
  const resetItems = RESET_CHOICES.map((value) => ({
    value,
    label: t(`organization:keys.form.reset.${value}`),
  }));
  const expiryItems = KEY_EXPIRY_CHOICES.map((value) => ({
    value,
    label: t(`organization:keys.form.expiry.${value}`),
  }));
  const lifetimeItems = LIFETIME_CHOICES.map((value) => ({
    value,
    label:
      value === "none"
        ? t("admin:keySettings.form.maxLifetime.none")
        : t(`organization:keys.form.expiry.${value}`),
  }));

  return (
    <form
      id="admin-key-settings-form"
      noValidate
      onSubmit={form.handleSubmit(onSubmit)}
      className="space-y-6"
    >
      <div className="grid gap-6 @lg:grid-cols-2">
        <Controller
          name="defaultLimitSar"
          control={form.control}
          render={({ field, fieldState }) => (
            <Field data-invalid={fieldState.invalid}>
              <FieldLabel htmlFor="admin-key-default-limit">
                {t("admin:keySettings.form.defaultLimit.label")}
              </FieldLabel>
              <div className="flex items-center gap-2">
                <span className="text-muted-foreground">SAR</span>
                <Input
                  id="admin-key-default-limit"
                  inputMode="decimal"
                  autoComplete="off"
                  placeholder={t("organization:keys.form.limit.none")}
                  {...field}
                  disabled={isDisabled}
                  aria-invalid={fieldState.invalid}
                />
              </div>
              <FieldDescription>
                {t("admin:keySettings.form.defaultLimit.description")}
              </FieldDescription>
              {fieldState.invalid && <FieldError errors={[fieldState.error]} />}
            </Field>
          )}
        />
        <Controller
          name="defaultLimitReset"
          control={form.control}
          render={({ field }) => (
            <Field>
              <FieldLabel htmlFor="admin-key-default-reset">
                {t("admin:keySettings.form.defaultReset.label")}
              </FieldLabel>
              <ChoiceSelect
                id="admin-key-default-reset"
                items={resetItems}
                value={field.value}
                onChange={field.onChange}
                disabled={isDisabled || defaultLimitSar.trim() === ""}
              />
              <FieldDescription>
                {t("admin:keySettings.form.defaultReset.description")}
              </FieldDescription>
            </Field>
          )}
        />
        <Controller
          name="defaultExpiry"
          control={form.control}
          render={({ field, fieldState }) => (
            <Field data-invalid={fieldState.invalid}>
              <FieldLabel htmlFor="admin-key-default-expiry">
                {t("admin:keySettings.form.defaultExpiry.label")}
              </FieldLabel>
              <ChoiceSelect
                id="admin-key-default-expiry"
                items={expiryItems}
                value={field.value}
                onChange={field.onChange}
                disabled={isDisabled}
              />
              <FieldDescription>
                {t("admin:keySettings.form.defaultExpiry.description")}
              </FieldDescription>
              {fieldState.invalid && <FieldError errors={[fieldState.error]} />}
            </Field>
          )}
        />
        <Controller
          name="maxLifetime"
          control={form.control}
          render={({ field }) => (
            <Field>
              <FieldLabel htmlFor="admin-key-max-lifetime">
                {t("admin:keySettings.form.maxLifetime.label")}
              </FieldLabel>
              <ChoiceSelect
                id="admin-key-max-lifetime"
                items={lifetimeItems}
                value={field.value}
                onChange={field.onChange}
                disabled={isDisabled}
              />
              <FieldDescription>
                {t("admin:keySettings.form.maxLifetime.description")}
              </FieldDescription>
            </Field>
          )}
        />
        <Controller
          name="presets"
          control={form.control}
          render={({ field, fieldState }) => (
            <Field data-invalid={fieldState.invalid}>
              <FieldLabel htmlFor="admin-key-presets">
                {t("admin:keySettings.form.presets.label")}
              </FieldLabel>
              <Input
                id="admin-key-presets"
                autoComplete="off"
                {...field}
                disabled={isDisabled}
                aria-invalid={fieldState.invalid}
              />
              <FieldDescription>
                {t("admin:keySettings.form.presets.description", {
                  count: MAX_KEY_LIMIT_PRESETS,
                })}
              </FieldDescription>
              {fieldState.invalid && <FieldError errors={[fieldState.error]} />}
            </Field>
          )}
        />
      </div>
      {canEdit && (
        <Button type="submit" disabled={isDisabled}>
          {form.formState.isSubmitting ? <Spinner /> : t("save")}
        </Button>
      )}
    </form>
  );
};

/** A select over a fixed list of choices, giving back only one of them. */
const ChoiceSelect = <T extends string>({
  id,
  items,
  value,
  onChange,
  disabled,
}: {
  readonly id: string;
  readonly items: readonly { value: T; label: string }[];
  readonly value: T;
  readonly onChange: (value: T) => void;
  readonly disabled: boolean;
}) => (
  <Select
    items={[...items]}
    value={value}
    onValueChange={(next) => {
      const found = items.find((item) => item.value === next);
      if (found) onChange(found.value);
    }}
    disabled={disabled}
  >
    <SelectTrigger id={id} className="w-full">
      <SelectValue />
    </SelectTrigger>
    <SelectContent>
      {items.map((item) => (
        <SelectItem key={item.value} value={item.value}>
          {item.label}
        </SelectItem>
      ))}
    </SelectContent>
  </Select>
);
