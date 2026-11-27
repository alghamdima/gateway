import * as z from "zod";

import {
  MAX_MODEL_NAME_LENGTH,
  MAX_MODEL_PRICE_NOTE_LENGTH,
  MAX_TOKEN_RATE_USD,
  MAX_UNIT_RATE_USD,
  MODEL_PRICE_SCALE,
  MODEL_PRICE_SUGGESTION_STATUSES,
} from "@workspace/db/schema";

import { isDecimalRate, isOtherRateField, normalizeDecimal } from "./rates";

/**
 * What the model price API accepts (ADMIN-09). The database checks the same
 * bounds (schema/model-prices.ts); these give a readable message first, and
 * refuse what Postgres would otherwise round (more decimals than the scale).
 */

const rateSchema = (ceiling: number) =>
  z
    .string()
    .trim()
    .refine(isDecimalRate, {
      message: `A rate is a plain decimal of USD with at most ${MODEL_PRICE_SCALE} decimals`,
    })
    .transform(normalizeDecimal)
    .refine((value) => Number(value) <= ceiling, {
      message: `A rate is at most ${ceiling} USD per unit`,
    });

const tokenRate = rateSchema(MAX_TOKEN_RATE_USD).nullable();

export const ratesSchema = z.object({
  inputUsd: tokenRate,
  outputUsd: tokenRate,
  cacheReadUsd: tokenRate,
  cacheWriteUsd: tokenRate,
  cacheWrite1hUsd: tokenRate,
  otherRates: z
    .record(z.string(), rateSchema(MAX_UNIT_RATE_USD))
    .refine((rates) => Object.keys(rates).every(isOtherRateField), {
      message: "Not a unit the gateway can price",
    })
    .default({}),
});

const nameSchema = z.string().trim().min(1).max(MAX_MODEL_NAME_LENGTH);
const noteSchema = z.string().trim().max(MAX_MODEL_PRICE_NOTE_LENGTH);

/**
 * A new version: a price for a provider's model, or for one provider key
 * (`providerKeyId`), or the end of a key's own price (`isRemoved`). With no
 * `effectiveFrom` it is in force now.
 */
export const createModelPriceSchema = z
  .object({
    provider: nameSchema,
    model: nameSchema,
    providerKeyId: nameSchema.nullable().default(null),
    effectiveFrom: z.coerce.date().optional(),
    isRemoved: z.boolean().default(false),
    rates: ratesSchema.optional(),
    note: noteSchema.default(""),
  })
  .superRefine((value, ctx) => {
    if (value.isRemoved) {
      if (value.providerKeyId === null) {
        ctx.addIssue({
          code: "custom",
          path: ["isRemoved"],
          message: "Only a key's own price can be removed",
        });
      }
      if (value.rates) {
        ctx.addIssue({
          code: "custom",
          path: ["rates"],
          message: "A removal carries no rates",
        });
      }
    } else if (!value.rates) {
      ctx.addIssue({
        code: "custom",
        path: ["rates"],
        message: "A price needs its rates",
      });
    }
  });

export type CreateModelPriceInput = z.infer<typeof createModelPriceSchema>;

export const listModelPricesQuerySchema = z.object({
  provider: nameSchema.optional(),
  q: z.string().trim().max(MAX_MODEL_NAME_LENGTH).optional(),
  /** Only the prices of provider keys (negotiated), or only provider-wide ones. */
  scope: z.enum(["provider", "key"]).optional(),
});

export const modelPriceHistoryQuerySchema = z.object({
  provider: nameSchema,
  model: nameSchema,
});

export const listSuggestionsQuerySchema = z.object({
  status: z.enum(MODEL_PRICE_SUGGESTION_STATUSES).default("pending"),
});

export const approveSuggestionSchema = z.object({
  effectiveFrom: z.coerce.date().optional(),
  reason: noteSchema.default(""),
});

export const rejectSuggestionSchema = z.object({
  reason: noteSchema.min(1, { message: "Say why the suggestion is rejected" }),
});

export const suggestionIdSchema = z.object({
  id: z.coerce.number().int().positive(),
});
