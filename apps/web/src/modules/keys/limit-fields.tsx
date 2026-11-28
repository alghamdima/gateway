"use client";

import * as z from "zod";

import { API_KEY_LIMIT_RESETS } from "@workspace/api/schema";
import { useTranslation } from "@workspace/i18n";
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

import { formatMicroSar, MICRO_SAR, sarToMicroSar } from "~/modules/keys/money";

import type { ApiKeyLimitReset } from "@workspace/api/schema";

export const RESETS = ["never", ...API_KEY_LIMIT_RESETS] as const;
export type Reset = (typeof RESETS)[number];

/** "none", "custom", or a quick pick's micro-SAR as a string. */
export interface LimitValues {
  limitChoice: string;
  /** Riyals, for a custom amount. */
  limitSar: string;
  reset: Reset;
}

export interface KeyLimitInput {
  limitMicroSar: number;
  reset: ApiKeyLimitReset | null;
}

const MAX_LIMIT_SAR = 4_000_000;

/** The rules of the limit fields, with the API's ceiling. */
export const limitValuesSchema = (invalid: string) =>
  z
    .object({
      limitChoice: z.string(),
      limitSar: z.string().trim(),
      reset: z.enum(RESETS),
    })
    .superRefine((value, ctx) => {
      if (value.limitChoice !== "custom") return;
      const sar = Number(value.limitSar);
      if (
        value.limitSar === "" ||
        !Number.isFinite(sar) ||
        sar <= 0 ||
        sar > MAX_LIMIT_SAR
      ) {
        ctx.addIssue({ code: "custom", path: ["limitSar"], message: invalid });
      }
    });

/** The limit in micro-SAR the fields stand for; null: no limit. */
export const limitMicroSarOf = (values: LimitValues) => {
  if (values.limitChoice === "none") return null;
  if (values.limitChoice === "custom") {
    const sar = Number(values.limitSar);
    return values.limitSar.trim() === "" || !Number.isFinite(sar) || sar <= 0
      ? null
      : sarToMicroSar(sar);
  }
  return Number(values.limitChoice);
};

/** What the API takes for the fields. */
export const toKeyLimit = (values: LimitValues): KeyLimitInput | null => {
  const limitMicroSar = limitMicroSarOf(values);
  return limitMicroSar === null
    ? null
    : {
        limitMicroSar,
        reset: values.reset === "never" ? null : values.reset,
      };
};

/** The fields for a limit the API returned, picking a quick pick when it is one. */
export const toLimitValues = (
  limitMicroSar: number | null,
  reset: string | null,
  presets: readonly number[],
): LimitValues => ({
  limitChoice:
    limitMicroSar === null
      ? "none"
      : presets.includes(limitMicroSar)
        ? String(limitMicroSar)
        : "custom",
  limitSar: limitMicroSar === null ? "" : String(limitMicroSar / MICRO_SAR),
  reset: RESETS.find((value) => value === reset) ?? "never",
});

interface LimitFieldsProps {
  readonly value: LimitValues;
  readonly onChange: (value: LimitValues) => void;
  /** The custom amount's error, if any. */
  readonly error?: string;
  readonly presets: readonly number[];
  readonly disabled: boolean;
  readonly idPrefix: string;
}

/**
 * Credit limit (a quick pick, a custom amount, or none) and its reset, with
 * the sentence OpenRouter shows under it (KEYS-02). Controlled: the form keeps
 * the three values as one field.
 */
export const LimitFields = ({
  value,
  onChange,
  error,
  presets,
  disabled,
  idPrefix,
}: LimitFieldsProps) => {
  const { t, i18n } = useTranslation(["common", "organization"]);
  const amount = limitMicroSarOf(value);
  const set = (change: Partial<LimitValues>) =>
    onChange({ ...value, ...change });

  const choices = [
    ...presets.map((preset) => ({
      value: String(preset),
      label: formatMicroSar(preset, i18n.language),
    })),
    { value: "custom", label: t("organization:keys.form.limit.custom") },
    { value: "none", label: t("organization:keys.form.limit.none") },
  ];
  const resets = RESETS.map((reset) => ({
    value: reset,
    label: t(`organization:keys.form.reset.${reset}`),
  }));

  const helper =
    amount === null
      ? t("organization:keys.form.limit.helperNone")
      : value.reset === "never"
        ? t("organization:keys.form.limit.helper", {
            amount: formatMicroSar(amount, i18n.language),
          })
        : t("organization:keys.form.limit.helperReset", {
            amount: formatMicroSar(amount, i18n.language),
            window: t(`organization:keys.form.reset.window.${value.reset}`),
          });

  return (
    <div className="space-y-4">
      <Field>
        <FieldLabel htmlFor={`${idPrefix}-limit-choice`}>
          {t("organization:keys.form.limit.label")}
        </FieldLabel>
        <Select
          items={choices}
          value={value.limitChoice}
          onValueChange={(choice) =>
            set({ limitChoice: typeof choice === "string" ? choice : "none" })
          }
          disabled={disabled}
        >
          <SelectTrigger id={`${idPrefix}-limit-choice`} className="w-full">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {choices.map((choice) => (
              <SelectItem key={choice.value} value={choice.value}>
                {choice.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </Field>
      {value.limitChoice === "custom" && (
        <Field data-invalid={error !== undefined}>
          <FieldLabel htmlFor={`${idPrefix}-limit-amount`}>
            {t("organization:keys.form.limit.amount")}
          </FieldLabel>
          <div className="flex items-center gap-2">
            <span className="text-muted-foreground">SAR</span>
            <Input
              id={`${idPrefix}-limit-amount`}
              inputMode="decimal"
              autoComplete="off"
              value={value.limitSar}
              onChange={(event) => set({ limitSar: event.target.value })}
              aria-invalid={error !== undefined}
              disabled={disabled}
            />
          </div>
          {error && <FieldError errors={[{ message: error }]} />}
        </Field>
      )}
      <FieldDescription>{helper}</FieldDescription>
      <Field>
        <FieldLabel htmlFor={`${idPrefix}-reset`}>
          {t("organization:keys.form.reset.label")}
        </FieldLabel>
        <Select
          items={resets}
          value={value.reset}
          onValueChange={(reset) =>
            set({ reset: RESETS.find((r) => r === reset) ?? "never" })
          }
          disabled={disabled || amount === null}
        >
          <SelectTrigger id={`${idPrefix}-reset`} className="w-full">
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
        <FieldDescription>
          {t("organization:keys.form.reset.description")}
        </FieldDescription>
      </Field>
    </div>
  );
};
