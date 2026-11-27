import * as z from "zod";

import {
  MAX_PROMO_MICRO_SAR,
  MAX_PROMO_NOTE_LENGTH,
  MAX_PROMO_ORGANIZATIONS,
  PROMO_CODE_KINDS,
  PROMO_CODE_PATTERN,
} from "@workspace/db/schema";
import { MICRO_SAR_PER_HALALA } from "@workspace/shared/utils";

import { normalizePromoCode, PROMO_CODE_STATUSES } from "./math";

/**
 * What the promo codes API accepts (BILL-08). The database checks the same
 * bounds (schema/promo-codes.ts); these give a readable message first.
 */

/** A code as typed: trimmed and upper-cased before it is checked. */
export const promoCodeInputSchema = z
  .string()
  .transform(normalizePromoCode)
  .pipe(
    z.string().regex(PROMO_CODE_PATTERN, {
      message: "A code is 3 to 32 letters, digits, hyphens or underscores",
    }),
  );

/** A code as a customer types it into a form: any text, checked by the server's rules. */
export const customerPromoCodeSchema = z.string().trim().min(1).max(64);

const halalas = (label: string) =>
  z
    .number()
    .int()
    .positive({ message: `${label} must be more than zero` })
    .max(MAX_PROMO_MICRO_SAR, {
      message: `${label} must be at most SAR 10,000`,
    })
    .refine((value) => value % MICRO_SAR_PER_HALALA === 0, {
      message: `${label} must be a whole number of halalas`,
    });

const bps = (label: string) =>
  z
    .number()
    .int()
    .min(1, { message: `${label} must be more than 0%` })
    .max(10_000, { message: `${label} must be at most 100%` });

export const promoCodeKindSchema = z.enum(PROMO_CODE_KINDS);

/** A code as staff create or edit it. Amounts not used by its kind must be null. */
export const savePromoCodeSchema = z
  .object({
    code: promoCodeInputSchema,
    kind: promoCodeKindSchema,
    feeDiscountBps: bps("The discount").nullable(),
    feeDiscountMicroSar: halalas("The discount").nullable(),
    bonusBps: bps("The bonus").nullable(),
    bonusCapMicroSar: halalas("The bonus cap").nullable(),
    creditMicroSar: halalas("The credit").nullable(),
    maxUses: z.number().int().positive().max(1_000_000).nullable(),
    onePerOrganization: z.boolean(),
    startsAt: z.coerce.date().nullable(),
    endsAt: z.coerce.date().nullable(),
    /** When the credit it grants expires (BILL-17); null: the purchase rule. */
    creditExpiresAt: z.coerce.date().nullable().default(null),
    minTopupMicroSar: halalas("The minimum top-up").nullable(),
    allOrganizations: z.boolean(),
    organizationIds: z
      .array(z.string().min(1))
      .max(MAX_PROMO_ORGANIZATIONS)
      .default([]),
    note: z.string().trim().max(MAX_PROMO_NOTE_LENGTH).default(""),
  })
  .superRefine((value, ctx) => {
    const issue = (path: string, message: string) =>
      ctx.addIssue({ code: "custom", path: [path], message });
    if (value.kind === "fee_discount") {
      if (
        (value.feeDiscountBps === null) ===
        (value.feeDiscountMicroSar === null)
      ) {
        issue(
          "feeDiscountBps",
          "Set either a percentage or a fixed amount off the fee",
        );
      }
    } else if (
      value.feeDiscountBps !== null ||
      value.feeDiscountMicroSar !== null
    ) {
      issue(
        "feeDiscountBps",
        "Only a fee discount takes something off the fee",
      );
    }
    if (value.kind === "topup_bonus") {
      if (value.bonusBps === null || value.bonusCapMicroSar === null) {
        issue("bonusBps", "A bonus needs a percentage and a cap");
      }
    } else if (value.bonusBps !== null || value.bonusCapMicroSar !== null) {
      issue("bonusBps", "Only a top-up bonus has a bonus percentage and cap");
    }
    if (value.kind === "free_credit") {
      if (value.creditMicroSar === null) {
        issue("creditMicroSar", "Free credit needs an amount");
      }
      if (value.minTopupMicroSar !== null) {
        issue("minTopupMicroSar", "Free credit is not tied to a top-up");
      }
    } else if (value.creditMicroSar !== null) {
      issue("creditMicroSar", "Only free credit grants a fixed amount");
    }
    if (
      value.startsAt &&
      value.endsAt &&
      value.endsAt.getTime() <= value.startsAt.getTime()
    ) {
      issue("endsAt", "The end must be after the start");
    }
    if (value.creditExpiresAt) {
      if (value.kind === "fee_discount") {
        issue("creditExpiresAt", "A fee discount grants no credit to expire");
      } else if (
        value.endsAt &&
        value.creditExpiresAt.getTime() < value.endsAt.getTime()
      ) {
        issue(
          "creditExpiresAt",
          "The credit must not expire before the code ends",
        );
      } else if (
        value.startsAt &&
        value.creditExpiresAt.getTime() <= value.startsAt.getTime()
      ) {
        issue("creditExpiresAt", "The credit must expire after the start");
      }
    }
    if (!value.allOrganizations && value.organizationIds.length === 0) {
      issue("organizationIds", "Choose at least one organization");
    }
  });

export type SavePromoCodeInput = z.infer<typeof savePromoCodeSchema>;

export const listPromoCodesQuerySchema = z.object({
  q: z.string().trim().max(64).optional(),
  status: z.enum(PROMO_CODE_STATUSES).optional(),
});

export const setPromoCodePausedSchema = z.object({ isPaused: z.boolean() });
