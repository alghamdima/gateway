import { createHash } from "node:crypto";

import { and, desc, eq } from "@workspace/db";
import { apiKey, gatewayCustomer } from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { getGateway } from "@workspace/gateway/server";
import { logger } from "@workspace/shared/logger";
import { generateId } from "@workspace/shared/utils";

import type { GatewayClient, LimitReset } from "@workspace/gateway/server";

/**
 * Organization API keys (docs/platform/adr/0004-key-model.md). Each one is a
 * virtual key in the gateway, under the organization's gateway customer. The
 * key itself goes to the caller once and is never stored here.
 */

export interface KeyLimit {
  /** micro-USD (1 USD = 1,000,000). */
  limitMicroUsd: number;
  /** null: the limit never resets. */
  reset: LimitReset | null;
}

/** What may be shown about a key: never its hash or virtual key id. */
const publicColumns = {
  id: apiKey.id,
  name: apiKey.name,
  prefix: apiKey.prefix,
  limitMicroUsd: apiKey.limitMicroUsd,
  limitReset: apiKey.limitReset,
  expiresAt: apiKey.expiresAt,
  disabledAt: apiKey.disabledAt,
  createdBy: apiKey.createdBy,
  createdAt: apiKey.createdAt,
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
  limit?: KeyLimit | null;
  expiresAt?: Date | null;
}

/**
 * Creates the virtual key first, then records it. If recording fails, the
 * virtual key is deleted, so no key the control plane does not know about
 * stays alive.
 */
export const createKey = async (
  { organizationId, userId, name, limit, expiresAt }: CreateKeyInput,
  gateway: GatewayClient = getGateway(),
) => {
  const customerId = await ensureGatewayCustomer(organizationId, gateway);
  // the gateway's key names are unique across every customer, so it gets our
  // id; the name the customer chose lives only here
  const id = generateId();
  const virtualKey = await gateway.createVirtualKey({
    name: id,
    customerId,
    expiresAt,
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
        limitMicroUsd: limit?.limitMicroUsd ?? null,
        limitReset: limit?.reset ?? null,
        expiresAt: expiresAt ?? null,
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

export const listKeys = async (organizationId: string) =>
  db
    .select(publicColumns)
    .from(apiKey)
    .where(eq(apiKey.organizationId, organizationId))
    .orderBy(desc(apiKey.createdAt));

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
            limitMicroUsd: input.limit?.limitMicroUsd ?? null,
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
