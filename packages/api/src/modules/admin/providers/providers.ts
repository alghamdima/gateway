import { eq } from "@workspace/db";
import { providerEndpoint } from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import {
  GatewayError,
  GatewayNotConfiguredError,
  getGateway,
} from "@workspace/gateway/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import { markPoliciesUnapplied } from "../../data-policy/pending";

import type { ProviderAgreement, ProviderRegion } from "@workspace/db/schema";
import type { GatewayClient } from "@workspace/gateway/server";

/**
 * The provider catalog (step D1 in docs/platform/PLAN.md, ADR 0015): every
 * provider key the gateway holds, with the data terms we know for it. The
 * gateway owns the keys and their secrets; our table only holds the terms.
 */

export interface ProviderTerms {
  region: ProviderRegion;
  /** 0: nothing kept. Null: unknown. */
  retentionDays: number | null;
  /** Null: unknown. */
  trainsOnData: boolean | null;
  agreement: ProviderAgreement;
  source: string;
  verifiedOn: string;
  updatedBy: string;
  updatedAt: Date;
}

export interface ProviderCatalogEntry {
  keyId: string;
  name: string;
  provider: string;
  isEnabled: boolean;
  /** False for terms whose key the gateway no longer has. */
  isInGateway: boolean;
  /** Null: nothing is known about this key. */
  terms: ProviderTerms | null;
  /** What a data policy may rely on. Unknown is the worst case (ADR 0015). */
  isZeroRetention: boolean;
  isNeverTrainedOn: boolean;
}

/** Unknown retention is not zero retention; unknown training is training. */
export const guarantees = (
  terms: Pick<ProviderTerms, "retentionDays" | "trainsOnData"> | null,
) => ({
  isZeroRetention: terms?.retentionDays === 0,
  isNeverTrainedOn: terms?.trainsOnData === false,
});

const termColumns = {
  keyId: providerEndpoint.keyId,
  provider: providerEndpoint.provider,
  region: providerEndpoint.region,
  retentionDays: providerEndpoint.retentionDays,
  trainsOnData: providerEndpoint.trainsOnData,
  agreement: providerEndpoint.agreement,
  source: providerEndpoint.source,
  verifiedOn: providerEndpoint.verifiedOn,
  updatedBy: providerEndpoint.updatedBy,
  updatedAt: providerEndpoint.updatedAt,
};

const gatewayKeys = async (gateway: GatewayClient) => {
  try {
    return await gateway.listProviderKeys();
  } catch (error) {
    if (
      error instanceof GatewayError ||
      error instanceof GatewayNotConfiguredError
    ) {
      throw new HttpException(HttpStatusCode.SERVICE_UNAVAILABLE, {
        code: "error.general",
      });
    }
    throw error;
  }
};

export const getProviderCatalog = async (
  gateway: GatewayClient = getGateway(),
): Promise<ProviderCatalogEntry[]> => {
  const [keys, rows] = await Promise.all([
    gatewayKeys(gateway),
    db.select(termColumns).from(providerEndpoint),
  ]);
  const termsOf = new Map(
    rows.map(({ keyId, provider: _provider, ...terms }) => [keyId, terms]),
  );

  const inGateway = keys.map((key): ProviderCatalogEntry => {
    const terms = termsOf.get(key.id) ?? null;
    return {
      keyId: key.id,
      name: key.name,
      provider: key.provider,
      isEnabled: key.isEnabled,
      isInGateway: true,
      terms,
      ...guarantees(terms),
    };
  });

  const known = new Set(keys.map((key) => key.id));
  const orphans = rows
    .filter((row) => !known.has(row.keyId))
    .map(({ keyId, provider, ...terms }): ProviderCatalogEntry => ({
      keyId,
      name: "",
      provider,
      isEnabled: false,
      isInGateway: false,
      terms,
      ...guarantees(terms),
    }));

  return [...inGateway, ...orphans].sort(
    (a, b) =>
      a.provider.localeCompare(b.provider) || a.name.localeCompare(b.name),
  );
};

export interface SetProviderTermsInput {
  keyId: string;
  staffId: string;
  region: ProviderRegion;
  retentionDays: number | null;
  trainsOnData: boolean | null;
  agreement: ProviderAgreement;
  source: string;
  verifiedOn: string;
}

/**
 * Terms can only be recorded for a key the gateway has. The change marks
 * every data policy as not applied in the same transaction: the caller then
 * pushes them (applyPendingPolicies).
 */
export const setProviderTerms = async (
  { keyId, staffId, ...terms }: SetProviderTermsInput,
  gateway: GatewayClient = getGateway(),
): Promise<ProviderTerms> => {
  const key = (await gatewayKeys(gateway)).find((k) => k.id === keyId);
  if (!key) {
    throw new HttpException(HttpStatusCode.NOT_FOUND, {
      code: "error.notFound",
    });
  }

  const values = {
    ...terms,
    provider: key.provider,
    updatedBy: staffId,
    updatedAt: new Date(),
  };
  const [row] = await db.transaction(async (tx) => {
    await markPoliciesUnapplied(tx);
    return tx
      .insert(providerEndpoint)
      .values({ keyId, ...values })
      .onConflictDoUpdate({ target: providerEndpoint.keyId, set: values })
      .returning(termColumns);
  });
  if (!row) {
    throw new Error("provider_endpoint upsert returned no row");
  }
  const { keyId: _keyId, provider: _provider, ...saved } = row;
  return saved;
};

/** Back to unknown. False when there were no terms to remove. */
export const removeProviderTerms = async (keyId: string) => {
  const removed = await db.transaction(async (tx) => {
    await markPoliciesUnapplied(tx);
    return tx
      .delete(providerEndpoint)
      .where(eq(providerEndpoint.keyId, keyId))
      .returning({ keyId: providerEndpoint.keyId });
  });
  return removed.length > 0;
};
