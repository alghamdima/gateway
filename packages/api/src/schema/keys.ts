import * as z from "zod";

import { API_KEY_LIMIT_RESETS } from "@workspace/db/schema";

/** 4,000,000 SAR in micro-SAR: a ceiling against typos. */
const MAX_LIMIT_MICRO_SAR = 4_000_000_000_000;

export const keyLimitSchema = z.object({
  /** micro-SAR (1 SAR = 1,000,000). */
  limitMicroSar: z.number().int().positive().max(MAX_LIMIT_MICRO_SAR),
  /** null: the limit never resets. */
  reset: z.enum(API_KEY_LIMIT_RESETS).nullable(),
});

const keyNameSchema = z.string().trim().min(1).max(100);

export const createApiKeySchema = z.object({
  name: keyNameSchema,
  limit: keyLimitSchema.nullable().optional(),
  expiresAt: z.coerce
    .date()
    .refine((date) => date.getTime() > Date.now(), {
      message: "expiresAt must be in the future",
    })
    .nullable()
    .optional(),
});

export const updateApiKeySchema = z
  .object({
    name: keyNameSchema.optional(),
    disabled: z.boolean().optional(),
    /** null removes the limit. */
    limit: keyLimitSchema.nullable().optional(),
  })
  .refine(
    (input) =>
      input.name !== undefined ||
      input.disabled !== undefined ||
      input.limit !== undefined,
    { message: "Nothing to update" },
  );

export type CreateApiKeyInput = z.infer<typeof createApiKeySchema>;
export type UpdateApiKeyInput = z.infer<typeof updateApiKeySchema>;
