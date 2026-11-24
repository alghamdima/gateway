import { and, desc, eq, gt, lte } from "@workspace/db";
import {
  organizationDataPolicyChange,
  providerEndpointChange,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";

/**
 * Where one request went and under which policy (step D3, ADR 0015). Built
 * from what is kept for good: the provider key on the ledger entry, and the
 * histories of that key's terms and of the organization's policy. Both are
 * read as of the moment of the request.
 */
export interface DataEvidence {
  /** The endpoint that served the request; null when the entry names no key. */
  endpoint: {
    provider: string;
    /** Null: no terms were recorded for the key when the request was made. */
    region: string | null;
    zero_retention: boolean;
    never_trained_on: boolean;
    verified_on: string | null;
    /**
     * True when the terms were edited after the request. The fields above
     * are the terms recorded at the time, not today's.
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
  const [recorded, later, change] = await Promise.all([
    // the terms as they stood when the request was made; an entry that records
    // their removal has none (the table's check guarantees it)
    providerKeyId
      ? db
          .select({
            provider: providerEndpointChange.provider,
            region: providerEndpointChange.region,
            retentionDays: providerEndpointChange.retentionDays,
            trainsOnData: providerEndpointChange.trainsOnData,
            verifiedOn: providerEndpointChange.verifiedOn,
          })
          .from(providerEndpointChange)
          .where(
            and(
              eq(providerEndpointChange.keyId, providerKeyId),
              lte(providerEndpointChange.changedAt, at),
            ),
          )
          .orderBy(
            desc(providerEndpointChange.changedAt),
            desc(providerEndpointChange.id),
          )
          .limit(1)
          .then((rows) => rows[0])
      : undefined,
    // whether they were edited afterwards
    providerKeyId
      ? db
          .select({ id: providerEndpointChange.id })
          .from(providerEndpointChange)
          .where(
            and(
              eq(providerEndpointChange.keyId, providerKeyId),
              gt(providerEndpointChange.changedAt, at),
            ),
          )
          .limit(1)
          .then((rows) => rows.length > 0)
      : false,
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
          provider: recorded?.provider ?? provider ?? "",
          region: recorded?.region ?? null,
          // unknown is the worst case, as in the policy itself
          zero_retention: recorded?.retentionDays === 0,
          never_trained_on: recorded?.trainsOnData === false,
          verified_on: recorded?.verifiedOn ?? null,
          terms_changed_since: later,
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
