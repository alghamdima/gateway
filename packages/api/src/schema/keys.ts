import * as z from "zod";

import {
  API_KEY_LIMIT_RESETS,
  KEY_EXPIRIES,
  MAX_KEY_LIMIT_MICRO_SAR,
  MAX_KEY_LIMIT_PRESETS,
} from "@workspace/db/schema";

import type { KeyExpiry } from "@workspace/db/schema";

/** The form checks the same values before sending. */
export {
  API_KEY_LIMIT_RESETS,
  KEY_EXPIRIES,
  KEY_EXPIRY_SECONDS,
  MAX_KEY_LIMIT_MICRO_SAR,
  MAX_KEY_LIMIT_PRESETS,
} from "@workspace/db/schema";
export type { ApiKeyLimitReset, KeyExpiry } from "@workspace/db/schema";

/** What the create form offers: a lifetime, or no expiration (KEYS-03). */
export const KEY_EXPIRY_CHOICES = [...KEY_EXPIRIES, "never"] as const;
export type KeyExpiryChoice = KeyExpiry | "never";

/** The answer to any attempt to change a key's expiry, as OpenRouter's. */
export const EXPIRY_IMMUTABLE_MESSAGE =
  "Expiration cannot be modified. Create a new API key for a new expiration time.";

const keyLimitAmountSchema = z
  .number()
  .int()
  .positive()
  .max(MAX_KEY_LIMIT_MICRO_SAR);

export const keyLimitSchema = z.object({
  /** micro-SAR (1 SAR = 1,000,000). */
  limitMicroSar: keyLimitAmountSchema,
  /** null: the limit never resets. */
  reset: z.enum(API_KEY_LIMIT_RESETS).nullable(),
});

const keyNameSchema = z.string().trim().min(1).max(100);

export const createApiKeySchema = z.object({
  name: keyNameSchema,
  /**
   * Required (KEYS-03): one of the lifetimes, or "never" when the platform's
   * maximum lifetime allows it (ADMIN-12). Counted from creation; fixed
   * afterwards.
   */
  expiry: z.enum(KEY_EXPIRY_CHOICES),
  /** Omitted: the platform's default limit. null: no limit. */
  limit: keyLimitSchema.nullable().optional(),
});

/**
 * Name, disabled and the limit may change. The expiry may not (KEYS-03): a
 * request that tries is refused with the reason, not silently ignored.
 */
export const updateApiKeySchema = z
  .object({
    name: keyNameSchema.optional(),
    disabled: z.boolean().optional(),
    /** null removes the limit. */
    limit: keyLimitSchema.nullable().optional(),
    expiresAt: z.unknown().optional(),
    expiry: z.unknown().optional(),
  })
  .superRefine((input, ctx) => {
    if (input.expiresAt !== undefined || input.expiry !== undefined) {
      ctx.addIssue({
        code: "custom",
        path: [input.expiresAt === undefined ? "expiry" : "expiresAt"],
        message: EXPIRY_IMMUTABLE_MESSAGE,
      });
      return;
    }
    if (
      input.name === undefined &&
      input.disabled === undefined &&
      input.limit === undefined
    ) {
      ctx.addIssue({ code: "custom", message: "Nothing to update" });
    }
  });

/**
 * A name, part of the shown prefix, or a whole key someone pasted (KEYS-05).
 * Sent in a POST body, never in a URL, so a pasted key stays out of access
 * logs and browser history.
 */
export const searchApiKeysSchema = z.object({
  query: z.string().trim().min(1).max(200),
});

/**
 * The key defaults staff set (ADMIN-12), with the database's rules on
 * key_settings: a reset needs a limit, and with a maximum lifetime the default
 * expiry must be a lifetime within it.
 */
export const updateKeySettingsSchema = z
  .object({
    defaultLimitMicroSar: keyLimitAmountSchema.nullable(),
    defaultLimitReset: z.enum(API_KEY_LIMIT_RESETS).nullable(),
    defaultExpiry: z.enum(KEY_EXPIRY_CHOICES),
    /** null: no maximum. */
    maxLifetime: z.enum(KEY_EXPIRIES).nullable(),
    limitPresetsMicroSar: z
      .array(keyLimitAmountSchema)
      .min(1)
      .max(MAX_KEY_LIMIT_PRESETS),
  })
  .superRefine((value, ctx) => {
    if (
      value.defaultLimitReset !== null &&
      value.defaultLimitMicroSar === null
    ) {
      ctx.addIssue({
        code: "custom",
        path: ["defaultLimitReset"],
        message: "A reset needs a default limit",
      });
    }
    if (value.maxLifetime === null) {
      return;
    }
    if (
      value.defaultExpiry === "never" ||
      KEY_EXPIRIES.indexOf(value.defaultExpiry) >
        KEY_EXPIRIES.indexOf(value.maxLifetime)
    ) {
      ctx.addIssue({
        code: "custom",
        path: ["defaultExpiry"],
        message: "The default expiry must be within the maximum lifetime",
      });
    }
  });

export type CreateApiKeyInput = z.infer<typeof createApiKeySchema>;
export type UpdateApiKeyInput = z.infer<typeof updateApiKeySchema>;
export type UpdateKeySettingsInput = z.infer<typeof updateKeySettingsSchema>;
