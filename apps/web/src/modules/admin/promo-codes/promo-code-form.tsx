"use client";

import { standardSchemaResolver } from "@hookform/resolvers/standard-schema";
import { useQueryClient } from "@tanstack/react-query";
import { useRouter } from "next/navigation";
import { Controller, useForm, useWatch } from "react-hook-form";
import * as z from "zod";

import { handle } from "@workspace/api/utils";
import { useTranslation } from "@workspace/i18n";
import { Button } from "@workspace/ui-web/button";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@workspace/ui-web/card";
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
} from "@workspace/ui-web/field";
import { Input } from "@workspace/ui-web/input";
import { Label } from "@workspace/ui-web/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@workspace/ui-web/select";
import { Spinner } from "@workspace/ui-web/spinner";
import { Switch } from "@workspace/ui-web/switch";
import { Textarea } from "@workspace/ui-web/textarea";
import { toast } from "@workspace/ui-web/toast";

import { pathsConfig } from "~/config/paths";
import { api } from "~/lib/api/client";
import {
  bpsToPercent,
  microSarToSar,
  percentToBps,
  sarToMicroSar,
} from "~/modules/admin/pricing/rates";

import type { PromoCode } from "~/modules/admin/promo-codes/format";

const KINDS = ["fee_discount", "topup_bonus", "free_credit"] as const;
const DISCOUNT_TYPES = ["percent", "fixed"] as const;
/** The API's bounds (schema/promo-codes.ts), as staff type them. */
const MAX_SAR = 10_000;
const MAX_NOTE_LENGTH = 500;
const CODE_PATTERN = /^[A-Za-z0-9_-]{3,32}$/;

const key = <Name extends string>(name: Name) =>
  `admin:promoCodes.form.${name}` as const;

interface PromoCodeFormProps {
  /** The code to edit; a new code without it. */
  readonly code?: PromoCode & { organizations: { id: string }[] };
  /** Once used, a code's kind cannot change (the API refuses it too). */
  readonly isKindLocked?: boolean;
  readonly onSaved?: () => void;
}

/**
 * Creates or edits a promo code (BILL-08). The API checks every rule again
 * and records the change with its values before and after.
 */
export const PromoCodeForm = ({
  code,
  isKindLocked = false,
  onSaved,
}: PromoCodeFormProps) => {
  const { t } = useTranslation(["common", "admin"]);
  const router = useRouter();
  const queryClient = useQueryClient();

  const amount = (required: boolean) =>
    z
      .string()
      .trim()
      .refine((value) => (value === "" ? !required : isSar(value)), {
        message: t(key("invalidAmount")),
      });
  const schema = z
    .object({
      code: z
        .string()
        .trim()
        .regex(CODE_PATTERN, {
          message: t(key("invalidCode")),
        }),
      kind: z.enum(KINDS),
      discountType: z.enum(DISCOUNT_TYPES),
      discountPercent: z.string().trim(),
      discountAmount: amount(false),
      bonusPercent: z.string().trim(),
      bonusCap: amount(false),
      credit: amount(false),
      maxUses: z
        .string()
        .trim()
        .refine((value) => value === "" || /^[1-9]\d{0,6}$/.test(value), {
          message: t(key("invalidUses")),
        }),
      onePerOrganization: z.boolean(),
      startsAt: z.string(),
      endsAt: z.string(),
      minTopup: amount(false),
      allOrganizations: z.boolean(),
      organizationIds: z.string(),
      note: z.string().trim().max(MAX_NOTE_LENGTH),
    })
    .superRefine((value, ctx) => {
      const issue = (path: string, message: string) =>
        ctx.addIssue({ code: "custom", path: [path], message });
      if (value.kind === "fee_discount") {
        if (
          value.discountType === "percent" &&
          !isPercent(value.discountPercent)
        ) {
          issue("discountPercent", t(key("invalidPercent")));
        }
        if (value.discountType === "fixed" && value.discountAmount === "") {
          issue("discountAmount", t(key("invalidAmount")));
        }
      }
      if (value.kind === "topup_bonus") {
        if (!isPercent(value.bonusPercent)) {
          issue("bonusPercent", t(key("invalidPercent")));
        }
        if (value.bonusCap === "") {
          issue("bonusCap", t(key("invalidAmount")));
        }
      }
      if (value.kind === "free_credit" && value.credit === "") {
        issue("credit", t(key("invalidAmount")));
      }
      if (
        value.startsAt &&
        value.endsAt &&
        endOfDay(value.endsAt) <= startOfDay(value.startsAt)
      ) {
        issue("endsAt", t(key("invalidDates")));
      }
      if (
        !value.allOrganizations &&
        idsOf(value.organizationIds).length === 0
      ) {
        issue("organizationIds", t(key("noOrganizations")));
      }
    });
  type Values = z.infer<typeof schema>;

  const form = useForm({
    resolver: standardSchemaResolver(schema),
    defaultValues: toFormValues(code),
  });
  const kind = useWatch({ control: form.control, name: "kind" });
  const discountType = useWatch({
    control: form.control,
    name: "discountType",
  });
  const allOrganizations = useWatch({
    control: form.control,
    name: "allOrganizations",
  });

  const save = async (values: Values) => {
    const json = toPayload(values);
    const saved = code
      ? await handle(api.admin["promo-codes"][":id"].$put)({
          param: { id: code.id },
          json,
        })
      : await handle(api.admin["promo-codes"].$post)({ json });
    await queryClient.invalidateQueries({ queryKey: ["admin", "promo-codes"] });
    toast.add({ title: t(key("saved")), type: "success" });
    if (onSaved) {
      onSaved();
      return;
    }
    router.push(pathsConfig.admin.promoCodes.code(saved.id));
  };

  const isBusy = form.formState.isSubmitting;
  const kinds = KINDS.map((value) => ({
    value,
    label: t(`admin:promoCodes.kind.${value}`),
  }));
  const discountTypes = DISCOUNT_TYPES.map((value) => ({
    value,
    label: t(key(value)),
  }));

  const text = (
    name:
      | "code"
      | "discountPercent"
      | "discountAmount"
      | "bonusPercent"
      | "bonusCap"
      | "credit"
      | "maxUses"
      | "minTopup"
      | "startsAt"
      | "endsAt",
    label: string,
    options: { placeholder?: string; type?: string; description?: string } = {},
  ) => (
    <Controller
      name={name}
      control={form.control}
      render={({ field, fieldState }) => (
        <Field data-invalid={fieldState.invalid} className="w-56">
          <FieldLabel htmlFor={`promo-${name}`}>{label}</FieldLabel>
          <Input
            id={`promo-${name}`}
            type={options.type ?? "text"}
            inputMode={options.type ? undefined : "decimal"}
            placeholder={options.placeholder}
            {...field}
            aria-invalid={fieldState.invalid}
            disabled={isBusy}
          />
          {options.description && (
            <FieldDescription>{options.description}</FieldDescription>
          )}
          {fieldState.invalid && <FieldError errors={[fieldState.error]} />}
        </Field>
      )}
    />
  );

  const toggle = (
    name: "onePerOrganization" | "allOrganizations",
    label: string,
  ) => (
    <Controller
      name={name}
      control={form.control}
      render={({ field }) => (
        <div className="flex items-center gap-3">
          <Switch
            id={`promo-${name}`}
            checked={field.value}
            onCheckedChange={field.onChange}
            disabled={isBusy}
          />
          <Label htmlFor={`promo-${name}`}>{label}</Label>
        </div>
      )}
    />
  );

  const form_ = (
    <form
      noValidate
      onSubmit={form.handleSubmit(save)}
      className="flex flex-col gap-6"
    >
      <div className="flex flex-wrap items-start gap-4">
        {text("code", t(key("code")), {
          description: t(key("codeDescription")),
        })}
        <Controller
          name="kind"
          control={form.control}
          render={({ field }) => (
            <Field className="w-56">
              <FieldLabel htmlFor="promo-kind">{t(key("kind"))}</FieldLabel>
              <Select
                items={kinds}
                value={field.value}
                onValueChange={field.onChange}
                disabled={isBusy || isKindLocked}
              >
                <SelectTrigger id="promo-kind" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {kinds.map((item) => (
                    <SelectItem key={item.value} value={item.value}>
                      {item.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {isKindLocked && (
                <FieldDescription>{t(key("kindLocked"))}</FieldDescription>
              )}
            </Field>
          )}
        />
      </div>

      {kind === "fee_discount" && (
        <div className="flex flex-wrap items-start gap-4">
          <Controller
            name="discountType"
            control={form.control}
            render={({ field }) => (
              <Field className="w-56">
                <FieldLabel htmlFor="promo-discount-type">
                  {t(key("discountType"))}
                </FieldLabel>
                <Select
                  items={discountTypes}
                  value={field.value}
                  onValueChange={field.onChange}
                  disabled={isBusy}
                >
                  <SelectTrigger id="promo-discount-type" className="w-full">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {discountTypes.map((item) => (
                      <SelectItem key={item.value} value={item.value}>
                        {item.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </Field>
            )}
          />
          {discountType === "percent"
            ? text("discountPercent", t(key("discountPercent")))
            : text("discountAmount", t(key("discountAmount")))}
        </div>
      )}
      {kind === "topup_bonus" && (
        <div className="flex flex-wrap items-start gap-4">
          {text("bonusPercent", t(key("bonusPercent")))}
          {text("bonusCap", t(key("bonusCap")))}
        </div>
      )}
      {kind === "free_credit" && (
        <div className="flex flex-wrap items-start gap-4">
          {text("credit", t(key("credit")))}
        </div>
      )}

      <div className="flex flex-wrap items-start gap-4">
        {text("maxUses", t(key("maxUses")), {
          placeholder: t(key("maxUsesPlaceholder")),
        })}
        {kind !== "free_credit" &&
          text("minTopup", t(key("minTopup")), {
            placeholder: t(key("minTopupPlaceholder")),
          })}
        {text("startsAt", t(key("startsAt")), { type: "date" })}
        {text("endsAt", t(key("endsAt")), { type: "date" })}
      </div>

      <div className="flex flex-col gap-3">
        {toggle("onePerOrganization", t(key("onePerOrganization")))}
        {toggle("allOrganizations", t(key("allOrganizations")))}
        {!allOrganizations && (
          <Controller
            name="organizationIds"
            control={form.control}
            render={({ field, fieldState }) => (
              <Field data-invalid={fieldState.invalid} className="max-w-xl">
                <FieldLabel htmlFor="promo-organization-ids">
                  {t(key("organizationIds"))}
                </FieldLabel>
                <Textarea
                  id="promo-organization-ids"
                  rows={4}
                  {...field}
                  aria-invalid={fieldState.invalid}
                  disabled={isBusy}
                />
                <FieldDescription>
                  {t(key("organizationIdsDescription"))}
                </FieldDescription>
                {fieldState.invalid && (
                  <FieldError errors={[fieldState.error]} />
                )}
              </Field>
            )}
          />
        )}
      </div>

      <Controller
        name="note"
        control={form.control}
        render={({ field, fieldState }) => (
          <Field data-invalid={fieldState.invalid} className="max-w-xl">
            <FieldLabel htmlFor="promo-note">{t(key("note"))}</FieldLabel>
            <Textarea
              id="promo-note"
              placeholder={t(key("notePlaceholder"))}
              maxLength={MAX_NOTE_LENGTH}
              {...field}
              aria-invalid={fieldState.invalid}
              disabled={isBusy}
            />
            {fieldState.invalid && <FieldError errors={[fieldState.error]} />}
          </Field>
        )}
      />

      <div>
        <Button type="submit" disabled={isBusy}>
          {isBusy ? <Spinner /> : code ? t("common:save") : t(key("create"))}
        </Button>
      </div>
    </form>
  );

  if (code) {
    return form_;
  }
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t(key("createTitle"))}</CardTitle>
      </CardHeader>
      <CardContent>{form_}</CardContent>
    </Card>
  );
};

/** Riyals with at most two decimals, above zero and within the API's bound. */
const isSar = (value: string) =>
  /^\d+(\.\d{1,2})?$/.test(value) &&
  Number(value) > 0 &&
  Number(value) <= MAX_SAR;

/** A percentage above zero, at most 100, with at most two decimals: whole basis points. */
const isPercent = (value: string) =>
  /^\d+(\.\d{1,2})?$/.test(value) && Number(value) > 0 && Number(value) <= 100;

const idsOf = (value: string) => [
  ...new Set(value.split(/[\s,]+/).filter(Boolean)),
];

const parts = (value: string) => {
  const [year, month, day] = value.split("-").map(Number);
  return { year: year ?? 0, month: (month ?? 1) - 1, day: day ?? 1 };
};
/** The start of `yyyy-mm-dd`, in the staff member's time zone. */
const startOfDay = (value: string) => {
  const { year, month, day } = parts(value);
  return new Date(year, month, day);
};
/** The start of the day after `yyyy-mm-dd`: the code is valid through that day. */
const endOfDay = (value: string) => {
  const { year, month, day } = parts(value);
  return new Date(year, month, day + 1);
};

const pad = (value: number) => String(value).padStart(2, "0");
const toDateInput = (date: Date) =>
  `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;

const sar = (microSar: number | null) =>
  microSar === null ? "" : String(microSarToSar(microSar));
const percent = (bps: number | null) =>
  bps === null ? "" : String(bpsToPercent(bps));
const microSarOrNull = (value: string) =>
  value === "" ? null : sarToMicroSar(Number(value));

const toFormValues = (
  code?: PromoCode & { organizations: { id: string }[] },
) => ({
  code: code?.code ?? "",
  kind: code?.kind ?? ("fee_discount" as const),
  discountType:
    code?.feeDiscountMicroSar != null
      ? ("fixed" as const)
      : ("percent" as const),
  discountPercent: percent(code?.feeDiscountBps ?? null),
  discountAmount: sar(code?.feeDiscountMicroSar ?? null),
  bonusPercent: percent(code?.bonusBps ?? null),
  bonusCap: sar(code?.bonusCapMicroSar ?? null),
  credit: sar(code?.creditMicroSar ?? null),
  maxUses: code?.maxUses == null ? "" : String(code.maxUses),
  onePerOrganization: code?.onePerOrganization ?? true,
  startsAt: code?.startsAt ? toDateInput(new Date(code.startsAt)) : "",
  // stored as the start of the day after the last valid one
  endsAt: code?.endsAt
    ? toDateInput(new Date(new Date(code.endsAt).getTime() - 1))
    : "",
  minTopup: sar(code?.minTopupMicroSar ?? null),
  allOrganizations: code?.allOrganizations ?? true,
  organizationIds: code?.organizations.map((org) => org.id).join("\n") ?? "",
  note: code?.note ?? "",
});

const toPayload = (values: {
  code: string;
  kind: (typeof KINDS)[number];
  discountType: (typeof DISCOUNT_TYPES)[number];
  discountPercent: string;
  discountAmount: string;
  bonusPercent: string;
  bonusCap: string;
  credit: string;
  maxUses: string;
  onePerOrganization: boolean;
  startsAt: string;
  endsAt: string;
  minTopup: string;
  allOrganizations: boolean;
  organizationIds: string;
  note: string;
}) => {
  const isDiscount = values.kind === "fee_discount";
  const isBonus = values.kind === "topup_bonus";
  const isFree = values.kind === "free_credit";
  return {
    code: values.code,
    kind: values.kind,
    feeDiscountBps:
      isDiscount && values.discountType === "percent"
        ? percentToBps(Number(values.discountPercent))
        : null,
    feeDiscountMicroSar:
      isDiscount && values.discountType === "fixed"
        ? microSarOrNull(values.discountAmount)
        : null,
    bonusBps: isBonus ? percentToBps(Number(values.bonusPercent)) : null,
    bonusCapMicroSar: isBonus ? microSarOrNull(values.bonusCap) : null,
    creditMicroSar: isFree ? microSarOrNull(values.credit) : null,
    maxUses: values.maxUses === "" ? null : Number(values.maxUses),
    onePerOrganization: values.onePerOrganization,
    startsAt: values.startsAt
      ? startOfDay(values.startsAt).toISOString()
      : null,
    endsAt: values.endsAt ? endOfDay(values.endsAt).toISOString() : null,
    minTopupMicroSar: isFree ? null : microSarOrNull(values.minTopup),
    allOrganizations: values.allOrganizations,
    organizationIds: values.allOrganizations
      ? []
      : idsOf(values.organizationIds),
    note: values.note,
  };
};
