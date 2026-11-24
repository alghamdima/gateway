import { and, desc, eq, lte } from "@workspace/db";
import {
  organizationDataPolicyChange,
  providerEndpoint,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";

/**
 * Where one request went and under which policy (step D3, ADR 0015). Built
 * from what is kept for good: the provider key on the ledger entry, the
 * terms recorded for that key, and the history of the organization's policy.
 */
export interface DataEvidence {
  /** The endpoint that served the request; null when the entry names no key. */
  endpoint: {
    provider: string;
    /** Null: no terms are recorded for the key. */
    region: string | null;
    zero_retention: boolean;
    never_trained_on: boolean;
    verified_on: string | null;
    /**
     * True when the terms were edited after the request: the fields above
     * are today's, not necessarily what was recorded then.
     */
    terms_changed_since: boolean;
  } | null;
  /** The organization's policy in force when the request was made. Version 0: none was set. */
  policy: {
    version: number;
    require_zero_retention: boolean;
    forbid_training: boolean;
    allowed_regions: string[] | null;
    allowed_providers: string[] | null;
  };
}

const NO_POLICY: DataEvidence["policy"] = {
  version: 0,
  require_zero_retention: false,
  forbid_training: false,
  allowed_regions: null,
  allowed_providers: null,
};

export interface EvidenceInput {
  organizationId: string;
  /** The gateway's provider key id from the ledger entry, if it has one. */
  providerKeyId: string | null;
  /** The provider name from the ledger entry, for a key with no terms. */
  provider: string | null;
  at: Date;
}

export const getDataEvidence = async ({
  organizationId,
  providerKeyId,
  provider,
  at,
}: EvidenceInput): Promise<DataEvidence> => {
  const [terms, change] = await Promise.all([
    providerKeyId
      ? db
          .select({
            provider: providerEndpoint.provider,
            region: providerEndpoint.region,
            retentionDays: providerEndpoint.retentionDays,
            trainsOnData: providerEndpoint.trainsOnData,
            verifiedOn: providerEndpoint.verifiedOn,
            updatedAt: providerEndpoint.updatedAt,
          })
          .from(providerEndpoint)
          .where(eq(providerEndpoint.keyId, providerKeyId))
          .then((rows) => rows[0])
      : undefined,
    db
      .select({
        version: organizationDataPolicyChange.version,
        requireZeroRetention: organizationDataPolicyChange.requireZeroRetention,
        forbidTraining: organizationDataPolicyChange.forbidTraining,
        allowedRegions: organizationDataPolicyChange.allowedRegions,
        allowedProviders: organizationDataPolicyChange.allowedProviders,
      })
      .from(organizationDataPolicyChange)
      .where(
        and(
          eq(organizationDataPolicyChange.organizationId, organizationId),
          lte(organizationDataPolicyChange.changedAt, at),
        ),
      )
      .orderBy(
        desc(organizationDataPolicyChange.changedAt),
        desc(organizationDataPolicyChange.id),
      )
      .limit(1)
      .then((rows) => rows[0]),
  ]);

  return {
    endpoint: providerKeyId
      ? {
          provider: terms?.provider ?? provider ?? "",
          region: terms?.region ?? null,
          // unknown is the worst case, as in the policy itself
          zero_retention: terms?.retentionDays === 0,
          never_trained_on: terms?.trainsOnData === false,
          verified_on: terms?.verifiedOn ?? null,
          terms_changed_since: terms ? terms.updatedAt > at : false,
        }
      : null,
    policy: change
      ? {
          version: change.version,
          require_zero_retention: change.requireZeroRetention,
          forbid_training: change.forbidTraining,
          allowed_regions: change.allowedRegions,
          allowed_providers: change.allowedProviders,
        }
      : NO_POLICY,
  };
};
