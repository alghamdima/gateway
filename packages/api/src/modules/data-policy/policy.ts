import { and, eq, lt, sql } from "@workspace/db";
import {
  apiKey,
  organization,
  organizationDataPolicy,
  organizationDataPolicyChange,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import {
  GatewayError,
  GatewayNotConfiguredError,
  getGateway,
} from "@workspace/gateway-client/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { logger } from "@workspace/shared/logger";
import { HttpException } from "@workspace/shared/utils";

import { getProviderCatalog } from "../admin/providers/providers";
import { accessFor, isOpen, OPEN_POLICY, satisfies } from "./eligibility";

import type { ProviderCatalogEntry } from "../admin/providers/providers";
import type { DataPolicy } from "./eligibility";
import type {
  GatewayClient,
  VirtualKeyAccess,
} from "@workspace/gateway-client/server";

/**
 * An organization's data policy and its enforcement (step D2 in
 * docs/platform/PLAN.md, ADR 0015). The policy is ours; the gateway enforces
 * it through the allowlist on each of the organization's keys.
 */

interface StoredPolicy extends DataPolicy {
  /** 0: never set. */
  version: number;
  appliedVersion: number;
}

const NEVER_SET: StoredPolicy = {
  ...OPEN_POLICY,
  version: 0,
  appliedVersion: 0,
};

const policyColumns = {
  requireZeroRetention: organizationDataPolicy.requireZeroRetention,
  forbidTraining: organizationDataPolicy.forbidTraining,
  allowedRegions: organizationDataPolicy.allowedRegions,
  allowedProviders: organizationDataPolicy.allowedProviders,
  version: organizationDataPolicy.version,
  appliedVersion: organizationDataPolicy.appliedVersion,
};

const storedPolicy = async (organizationId: string): Promise<StoredPolicy> => {
  const [row] = await db
    .select(policyColumns)
    .from(organizationDataPolicy)
    .where(eq(organizationDataPolicy.organizationId, organizationId));
  return row ?? NEVER_SET;
};

const gatewayUnavailable = (error: unknown): never => {
  if (
    error instanceof GatewayError ||
    error instanceof GatewayNotConfiguredError
  ) {
    throw new HttpException(HttpStatusCode.SERVICE_UNAVAILABLE, {
      code: "error.general",
    });
  }
  throw error;
};

/**
 * What a new key of the organization may reach. The catalog is only read
 * when the policy restricts something.
 */
export const accessForOrganization = async (
  organizationId: string,
  gateway: GatewayClient = getGateway(),
): Promise<VirtualKeyAccess> => {
  const policy = await storedPolicy(organizationId);
  return isOpen(policy)
    ? { allowAll: true }
    : accessFor(policy, await getProviderCatalog(gateway));
};

/** One endpoint as the organization may see it: no key names or ids. */
export interface PolicyEndpoint {
  provider: string;
  region: string | null;
  isZeroRetention: boolean;
  isNeverTrainedOn: boolean;
  isAllowed: boolean;
}

export interface OrganizationDataPolicy extends DataPolicy {
  version: number;
  /** False while a change is being pushed to the keys, or after a push failed. */
  isApplied: boolean;
  endpoints: PolicyEndpoint[];
}

const describe = (
  policy: StoredPolicy,
  catalog: ProviderCatalogEntry[],
): OrganizationDataPolicy => ({
  requireZeroRetention: policy.requireZeroRetention,
  forbidTraining: policy.forbidTraining,
  allowedRegions: policy.allowedRegions,
  allowedProviders: policy.allowedProviders,
  version: policy.version,
  isApplied: policy.appliedVersion === policy.version,
  endpoints: catalog
    .filter((entry) => entry.isInGateway && entry.isEnabled)
    .map((entry) => ({
      provider: entry.provider,
      region: entry.terms?.region ?? null,
      isZeroRetention: entry.isZeroRetention,
      isNeverTrainedOn: entry.isNeverTrainedOn,
      isAllowed: satisfies(entry, policy),
    })),
});

export const getDataPolicy = async (
  organizationId: string,
  gateway: GatewayClient = getGateway(),
): Promise<OrganizationDataPolicy> => {
  const [policy, catalog] = await Promise.all([
    storedPolicy(organizationId),
    getProviderCatalog(gateway),
  ]);
  return describe(policy, catalog);
};

/**
 * Writes the allowlist the policy implies to every key of the organization,
 * then records that the keys carry this version. If the gateway fails
 * partway, the version stays unapplied and the next call starts over: each
 * write replaces a key's whole list, so repeating it is safe.
 */
const pushToKeys = async (
  organizationId: string,
  policy: StoredPolicy,
  catalog: ProviderCatalogEntry[],
  gateway: GatewayClient,
) => {
  const access = accessFor(policy, catalog);
  const keys = await db
    .select({ virtualKeyId: apiKey.virtualKeyId })
    .from(apiKey)
    .where(eq(apiKey.organizationId, organizationId));
  for (const key of keys) {
    await gateway.setVirtualKeyAccess(key.virtualKeyId, access);
  }
  // only if the policy has not changed meanwhile: a newer version is pushed by its own call
  await db
    .update(organizationDataPolicy)
    .set({ appliedVersion: policy.version })
    .where(
      and(
        eq(organizationDataPolicy.organizationId, organizationId),
        eq(organizationDataPolicy.version, policy.version),
      ),
    );
};

export interface SetDataPolicyInput extends DataPolicy {
  organizationId: string;
  userId: string;
}

/**
 * Records the policy as a new version, then pushes it to the organization's
 * keys. If the push fails the policy is recorded but not applied, and the
 * caller gets 503: saving again, or the scheduled re-apply, finishes it.
 */
export const setDataPolicy = async (
  { organizationId, userId, ...policy }: SetDataPolicyInput,
  gateway: GatewayClient = getGateway(),
): Promise<OrganizationDataPolicy> => {
  const [org] = await db
    .select({ id: organization.id })
    .from(organization)
    .where(eq(organization.id, organizationId));
  if (!org) {
    throw new HttpException(HttpStatusCode.NOT_FOUND, {
      code: "error.notFound",
    });
  }

  // read first: with the gateway down, nothing is recorded that cannot be pushed
  const catalog = await getProviderCatalog(gateway);
  const values = { ...policy, updatedBy: userId, updatedAt: new Date() };
  // the version and its entry in the history are written together
  const row = await db.transaction(async (tx) => {
    const [saved] = await tx
      .insert(organizationDataPolicy)
      .values({ organizationId, ...values })
      .onConflictDoUpdate({
        target: organizationDataPolicy.organizationId,
        set: {
          ...values,
          version: sql`${organizationDataPolicy.version} + 1`,
        },
      })
      .returning(policyColumns);
    if (saved) {
      await tx.insert(organizationDataPolicyChange).values({
        organizationId,
        version: saved.version,
        ...policy,
        changedBy: userId,
        changedAt: values.updatedAt,
      });
    }
    return saved;
  });
  if (!row) {
    throw new Error("organization_data_policy upsert returned no row");
  }

  try {
    await pushToKeys(organizationId, row, catalog, gateway);
  } catch (error) {
    gatewayUnavailable(error);
  }
  return describe({ ...row, appliedVersion: row.version }, catalog);
};

export interface ApplyResult {
  applied: number;
  failed: string[];
}

/**
 * Pushes every policy whose keys are not known to carry it. One failing
 * organization does not stop the others; it stays unapplied for the next run.
 */
export const applyPendingPolicies = async (
  gateway: GatewayClient = getGateway(),
): Promise<ApplyResult> => {
  const pending = await db
    .select({
      organizationId: organizationDataPolicy.organizationId,
      ...policyColumns,
    })
    .from(organizationDataPolicy)
    .where(
      lt(organizationDataPolicy.appliedVersion, organizationDataPolicy.version),
    );
  if (pending.length === 0) {
    return { applied: 0, failed: [] };
  }

  const result: ApplyResult = { applied: 0, failed: [] };
  let catalog: ProviderCatalogEntry[];
  try {
    catalog = await getProviderCatalog(gateway);
  } catch (error) {
    logger.error("data policy: the provider catalog could not be read", error);
    return { applied: 0, failed: pending.map((row) => row.organizationId) };
  }
  for (const { organizationId, ...policy } of pending) {
    try {
      await pushToKeys(organizationId, policy, catalog, gateway);
      result.applied++;
    } catch (error) {
      logger.error(
        `data policy: could not apply the policy of ${organizationId}`,
        error,
      );
      result.failed.push(organizationId);
    }
  }
  return result;
};
