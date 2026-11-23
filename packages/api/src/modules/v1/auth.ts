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
  limitMicroUsd: number | null;
  limitReset: string | null;
  expiresAt: Date | null;
  createdAt: Date;
}

export type KeyAuthResult =
  | { isAuthenticated: true; key: CallingKey }
  | { isAuthenticated: false; message: string };

export const AUTH_MESSAGES = {
  missing: "No auth credentials found",
  malformed: "Malformed Authorization header: expected 'Bearer <key>'",
  unknown: "Invalid API key",
  disabled: "This API key is disabled",
  expired: "This API key has expired",
} as const;

/** The key a request carries, or why it carries none. */
export const readKey = (headers: {
  authorization?: string | null;
  apiKey?: string | null;
}): { key: string } | { message: string } => {
  const header = headers.authorization?.trim();
  if (header) {
    const match = /^Bearer\s+(\S+)$/i.exec(header);
    return match?.[1]
      ? { key: match[1] }
      : { message: AUTH_MESSAGES.malformed };
  }
  const fromApiKey = headers.apiKey?.trim();
  return fromApiKey ? { key: fromApiKey } : { message: AUTH_MESSAGES.missing };
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
      limitMicroUsd: apiKey.limitMicroUsd,
      limitReset: apiKey.limitReset,
      expiresAt: apiKey.expiresAt,
      disabledAt: apiKey.disabledAt,
      createdAt: apiKey.createdAt,
    })
    .from(apiKey)
    .where(eq(apiKey.valueHash, hashKey(value)));

  if (!row) {
    return { isAuthenticated: false, message: AUTH_MESSAGES.unknown };
  }
  if (row.disabledAt) {
    return { isAuthenticated: false, message: AUTH_MESSAGES.disabled };
  }
  if (row.expiresAt && row.expiresAt <= now) {
    return { isAuthenticated: false, message: AUTH_MESSAGES.expired };
  }
  const { disabledAt: _disabledAt, ...key } = row;
  return { isAuthenticated: true, key };
};
