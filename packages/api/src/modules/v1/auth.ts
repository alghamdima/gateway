import { eq } from "@workspace/db";
import { apiKey } from "@workspace/db/schema";
import { db } from "@workspace/db/server";

import { hashKey } from "../keys/keys";

/**
 * Who is calling the public API: an organization API key sent as
 * `Authorization: Bearer <key>` or, for Anthropic SDK clients, `x-api-key`.
 * Each failure has its own message, as OpenRouter's (research brief 02).
 */

export interface CallingKey {
  id: string;
  organizationId: string;
  virtualKeyId: string;
  name: string;
  prefix: string;
  limitMicroSar: number | null;
  limitReset: string | null;
  expiresAt: Date | null;
  createdAt: Date;
}

/**
 * Why a key was refused: the `reason` of the error contract under
 * `limit_source: "key"`, the same values the gateway gives on the inference
 * routes (docs/platform/api-errors.md).
 */
export type KeyRefusalReason =
  | "key_missing"
  | "key_invalid"
  | "key_disabled"
  | "key_expired";

export interface KeyRefusal {
  message: string;
  reason: KeyRefusalReason;
}

export type KeyAuthResult =
  | { isAuthenticated: true; key: CallingKey }
  | ({ isAuthenticated: false } & KeyRefusal);

export const AUTH_MESSAGES = {
  missing: "No auth credentials found",
  malformed: "Malformed Authorization header: expected 'Bearer <key>'",
  unknown: "Invalid API key",
  disabled: "This API key is disabled",
  expired: "This API key has expired",
} as const;

/** What to do about each refusal, for a person. */
export const KEY_REMEDY_HINTS: Record<KeyRefusalReason, string> = {
  key_missing: "Send your API key as Authorization: Bearer <key>.",
  key_invalid:
    "The API key does not exist or was deleted. Check the key, or create a new one in the dashboard.",
  key_disabled:
    "This API key is disabled. Enable it again in the dashboard, or use another key.",
  key_expired:
    "This API key has expired, and its expiry cannot be changed. Create a new key in the dashboard.",
};

/** The key a request carries, or why it carries none. */
export const readKey = (headers: {
  authorization?: string | null;
  apiKey?: string | null;
}): { key: string } | KeyRefusal => {
  const header = headers.authorization?.trim();
  if (header) {
    const match = /^Bearer\s+(\S+)$/i.exec(header);
    return match?.[1]
      ? { key: match[1] }
      : { message: AUTH_MESSAGES.malformed, reason: "key_invalid" };
  }
  const fromApiKey = headers.apiKey?.trim();
  return fromApiKey
    ? { key: fromApiKey }
    : { message: AUTH_MESSAGES.missing, reason: "key_missing" };
};

export const authenticateKey = async (
  value: string,
  now = new Date(),
): Promise<KeyAuthResult> => {
  const [row] = await db
    .select({
      id: apiKey.id,
      organizationId: apiKey.organizationId,
      virtualKeyId: apiKey.virtualKeyId,
      name: apiKey.name,
      prefix: apiKey.prefix,
      limitMicroSar: apiKey.limitMicroSar,
      limitReset: apiKey.limitReset,
      expiresAt: apiKey.expiresAt,
      disabledAt: apiKey.disabledAt,
      createdAt: apiKey.createdAt,
    })
    .from(apiKey)
    .where(eq(apiKey.valueHash, hashKey(value)));

  if (!row) {
    return {
      isAuthenticated: false,
      message: AUTH_MESSAGES.unknown,
      reason: "key_invalid",
    };
  }
  if (row.disabledAt) {
    return {
      isAuthenticated: false,
      message: AUTH_MESSAGES.disabled,
      reason: "key_disabled",
    };
  }
  if (row.expiresAt && row.expiresAt <= now) {
    return {
      isAuthenticated: false,
      message: AUTH_MESSAGES.expired,
      reason: "key_expired",
    };
  }
  const { disabledAt: _disabledAt, ...key } = row;
  return { isAuthenticated: true, key };
};
