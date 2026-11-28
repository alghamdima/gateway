import { createHash } from "node:crypto";

import { and, desc, eq, ilike, or, sql } from "@workspace/db";
import { apiKey, gatewayCustomer } from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { getGateway } from "@workspace/gateway-client/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { logger } from "@workspace/shared/logger";
import { generateId, HttpException } from "@workspace/shared/utils";

import { accessForOrganization } from "../data-policy/policy";
import { expiresAtFor, isWithinMaxLifetime } from "./expiry";
import { getKeySettings } from "./settings";
import {
  getKeySpendByDay,
  getKeyUsage,
  getOrganizationKeyUsage,
} from "./usage";

import type {
  GatewayClient,
  LimitReset,
} from "@workspace/gateway-client/server";

/**
 * Organization API keys (docs/platform/adr/0004-key-model.md). Each one is a
 * virtual key in the gateway, under the organization's gateway customer. The
 * key itself goes to the caller once and is never stored here.
 */

export interface KeyLimit {
  /** micro-SAR (1 SAR = 1,000,000). */
  limitMicroSar: number;
  /** null: the limit never resets. */
  reset: LimitReset | null;
}

/** What may be shown about a key: never its hash or virtual key id. */
export const publicColumns = {
  id: apiKey.id,
  name: apiKey.name,
  prefix: apiKey.prefix,
  limitMicroSar: apiKey.limitMicroSar,
  limitReset: apiKey.limitReset,
  expiresAt: apiKey.expiresAt,
  disabledAt: apiKey.disabledAt,
  createdBy: apiKey.createdBy,
  createdAt: apiKey.createdAt,
};

/**
 * Enabled, disabled, or expired (KEYS-04). An expired key cannot be enabled
 * again in any way that helps: its expiry is fixed (KEYS-03), so expired wins.
 */
export type KeyStatus = "enabled" | "disabled" | "expired";

export const keyStatus = (
  key: { disabledAt: Date | null; expiresAt: Date | null },
  now = new Date(),
): KeyStatus => {
  if (key.expiresAt && key.expiresAt.getTime() <= now.getTime()) {
    return "expired";
  }
  return key.disabledAt ? "disabled" : "enabled";
};

export const hashKey = (value: string) =>
  createHash("sha256").update(value).digest("hex");

/** Enough to tell keys apart, not enough to use one: "sk-bf-" plus 6 characters. */
export const keyPrefix = (value: string) => value.slice(0, 12);

/**
 * The organization's gateway customer, created on first use. Two requests
 * racing to create it both ask the gateway; the one that loses the insert
 * deletes its customer again.
 */
export const ensureGatewayCustomer = async (
  organizationId: string,
  gateway: GatewayClient = getGateway(),
) => {
  const [existing] = await db
    .select({ customerId: gatewayCustomer.customerId })
    .from(gatewayCustomer)
    .where(eq(gatewayCustomer.organizationId, organizationId));
  if (existing) {
    return existing.customerId;
  }

  // named by organization id, so an operator can find it in the gateway
  const created = await gateway.createCustomer(organizationId);
  const [inserted] = await db
    .insert(gatewayCustomer)
    .values({ organizationId, customerId: created.id })
    .onConflictDoNothing({ target: gatewayCustomer.organizationId })
    .returning({ customerId: gatewayCustomer.customerId });
  if (inserted) {
    return inserted.customerId;
  }

  await gateway.deleteCustomer(created.id).catch((error: unknown) =>
    logger.error("gateway customer left behind after a lost race", {
      organizationId,
      customerId: created.id,
      error,
    }),
  );
  const [winner] = await db
    .select({ customerId: gatewayCustomer.customerId })
    .from(gatewayCustomer)
    .where(eq(gatewayCustomer.organizationId, organizationId));
  if (!winner) {
    throw new Error(`no gateway customer for organization ${organizationId}`);
  }
  return winner.customerId;
};

export interface CreateKeyInput {
  organizationId: string;
  userId: string;
  name: string;
  /** Omitted: the platform's default limit (ADMIN-12). null: no limit. */
  limit?: KeyLimit | null;
  /**
   * Omitted: the platform's default expiry (ADMIN-12). null: no expiration.
   * The API turns the customer's choice into this moment (expiresAtFor); it
   * must be within the platform's maximum lifetime.
   */
  expiresAt?: Date | null;
}

/** A key may not outlive the platform's maximum lifetime, nor skip expiring while one is set. */
const expiryBeyondMaximum = () =>
  new HttpException(HttpStatusCode.UNPROCESSABLE_ENTITY, {
    code: "organization:keys.error.expiryBeyondMaxLifetime",
  });

/**
 * Creates the virtual key first, then records it. If recording fails, the
 * virtual key is deleted, so no key the control plane does not know about
 * stays alive. The expiry given to the gateway is the expiry recorded here:
 * the gateway enforces it, and nothing changes it later but the platform's
 * maximum lifetime (lifetime.ts), which updates both.
 */
export const createKey = async (
  { organizationId, userId, name, limit, expiresAt }: CreateKeyInput,
  gateway: GatewayClient = getGateway(),
) => {
  const settings = await getKeySettings();
  const now = new Date();
  const expiry =
    expiresAt === undefined
      ? expiresAtFor(settings.defaultExpiry, now)
      : expiresAt;
  if (!isWithinMaxLifetime(expiry, settings.maxLifetimeSeconds, now)) {
    throw expiryBeyondMaximum();
  }
  const keyLimit =
    limit === undefined
      ? settings.defaultLimitMicroSar === null
        ? null
        : {
            limitMicroSar: settings.defaultLimitMicroSar,
            reset: settings.defaultLimitReset,
          }
      : limit;

  const customerId = await ensureGatewayCustomer(organizationId, gateway);
  // the gateway's key names are unique across every customer, so it gets our
  // id; the name the customer chose lives only here
  const id = generateId();
  const virtualKey = await gateway.createVirtualKey({
    name: id,
    customerId,
    expiresAt: expiry,
    // the organization's data policy, from the key's first request (ADR 0015)
    access: await accessForOrganization(organizationId, gateway),
  });

  try {
    const [row] = await db
      .insert(apiKey)
      .values({
        id,
        organizationId,
        createdBy: userId,
        name,
        virtualKeyId: virtualKey.id,
        prefix: keyPrefix(virtualKey.value),
        valueHash: hashKey(virtualKey.value),
        limitMicroSar: keyLimit?.limitMicroSar ?? null,
        limitReset: keyLimit?.reset ?? null,
        expiresAt: expiry,
        createdAt: now,
      })
      .returning(publicColumns);
    if (!row) {
      throw new Error("api_key insert returned no row");
    }
    return { key: virtualKey.value, apiKey: row };
  } catch (error) {
    await gateway
      .deleteVirtualKey(virtualKey.id)
      .catch((cleanupError: unknown) =>
        // the key exists in the gateway with no record here: an operator must delete it
        logger.error("virtual key left behind after a failed create", {
          organizationId,
          virtualKeyId: virtualKey.id,
          error: cleanupError,
        }),
      );
    throw error;
  }
};

/**
 * The organization's keys, newest first, each with its status and usage
 * (KEYS-06): spend all time, what counts against its limit now, last used.
 */
export const listKeys = async (organizationId: string) => {
  const [rows, usage] = await Promise.all([
    db
      .select(publicColumns)
      .from(apiKey)
      .where(eq(apiKey.organizationId, organizationId))
      .orderBy(desc(apiKey.createdAt)),
    getOrganizationKeyUsage(organizationId),
  ]);
  const now = new Date();
  return rows.map((row) => ({
    ...row,
    status: keyStatus(row, now),
    usage: usage.get(row.id) ?? {
      total: 0,
      spentAgainstLimit: 0,
      lastUsedAt: null,
    },
  }));
};

/**
 * Finds keys by name, by the start of their shown prefix, or by a whole key
 * someone pasted (KEYS-05). A pasted key is hashed and compared with the
 * stored hash: the key itself is never stored, logged or sent back, and only
 * the organization's own keys can match.
 */
export const searchKeys = async (organizationId: string, query: string) => {
  const term = query.trim();
  const escaped = term.replace(/[\\%_]/g, (char) => `\\${char}`);
  const matches = await db
    .select({ id: apiKey.id })
    .from(apiKey)
    .where(
      and(
        eq(apiKey.organizationId, organizationId),
        or(
          eq(apiKey.valueHash, hashKey(term)),
          ilike(apiKey.name, `%${escaped}%`),
          sql`starts_with(${apiKey.prefix}, ${term})`,
        ),
      ),
    );
  const ids = new Set(matches.map((match) => match.id));
  return (await listKeys(organizationId)).filter((key) => ids.has(key.id));
};

/** One key with everything its page shows (KEYS-11); undefined when the organization has no such key. */
export const getKey = async (organizationId: string, keyId: string) => {
  const [row] = await db
    .select({ ...publicColumns, virtualKeyId: apiKey.virtualKeyId })
    .from(apiKey)
    .where(
      and(eq(apiKey.id, keyId), eq(apiKey.organizationId, organizationId)),
    );
  if (!row) {
    return undefined;
  }
  const { virtualKeyId, ...key } = row;
  const [usage, spendByDay] = await Promise.all([
    getKeyUsage(virtualKeyId, key.limitReset),
    getKeySpendByDay(virtualKeyId),
  ]);
  return { ...key, status: keyStatus(key), usage, spendByDay };
};

const findKey = async (organizationId: string, keyId: string) => {
  const [row] = await db
    .select({ virtualKeyId: apiKey.virtualKeyId })
    .from(apiKey)
    .where(
      and(eq(apiKey.id, keyId), eq(apiKey.organizationId, organizationId)),
    );
  return row;
};

export interface UpdateKeyInput {
  name?: string;
  disabled?: boolean;
  /** null removes the limit. */
  limit?: KeyLimit | null;
}

/**
 * The gateway first: disabling must take effect even if recording it fails,
 * and a retry repeats the same change. The spending limit lives only here:
 * the credits plugin reads it on every hold (ADR 0013). Returns undefined
 * when the organization has no such key.
 */
export const updateKey = async (
  organizationId: string,
  keyId: string,
  input: UpdateKeyInput,
  gateway: GatewayClient = getGateway(),
) => {
  const found = await findKey(organizationId, keyId);
  if (!found) {
    return undefined;
  }
  // the name lives only here (createKey): only disabling reaches the gateway
  if (input.disabled !== undefined) {
    await gateway.updateVirtualKey(found.virtualKeyId, {
      isActive: !input.disabled,
    });
  }
  const [row] = await db
    .update(apiKey)
    .set({
      ...(input.name === undefined ? {} : { name: input.name }),
      ...(input.disabled === undefined
        ? {}
        : { disabledAt: input.disabled ? new Date() : null }),
      ...(input.limit === undefined
        ? {}
        : {
            limitMicroSar: input.limit?.limitMicroSar ?? null,
            limitReset: input.limit?.reset ?? null,
          }),
    })
    .where(and(eq(apiKey.id, keyId), eq(apiKey.organizationId, organizationId)))
    .returning(publicColumns);
  return row;
};

/** Deleted in the gateway first, so it stops working before the record goes. */
export const deleteKey = async (
  organizationId: string,
  keyId: string,
  gateway: GatewayClient = getGateway(),
) => {
  const found = await findKey(organizationId, keyId);
  if (!found) {
    return false;
  }
  await gateway.deleteVirtualKey(found.virtualKeyId);
  await db
    .delete(apiKey)
    .where(
      and(eq(apiKey.id, keyId), eq(apiKey.organizationId, organizationId)),
    );
  return true;
};
