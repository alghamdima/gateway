import type { ProviderCatalogEntry } from "../admin/providers/providers";
import type { ProviderRegion } from "@workspace/db/schema";
import type { VirtualKeyAccess } from "@workspace/gateway/server";

/**
 * What an organization requires of the endpoints that serve it
 * (docs/platform/adr/0015-data-policy.md). A null list allows every region
 * or every provider.
 */
export interface DataPolicy {
  requireZeroRetention: boolean;
  forbidTraining: boolean;
  allowedRegions: ProviderRegion[] | null;
  allowedProviders: string[] | null;
}

/** The policy of an organization that never set one. */
export const OPEN_POLICY: DataPolicy = {
  requireZeroRetention: false,
  forbidTraining: false,
  allowedRegions: null,
  allowedProviders: null,
};

export const isOpen = (policy: DataPolicy) =>
  !policy.requireZeroRetention &&
  !policy.forbidTraining &&
  policy.allowedRegions === null &&
  policy.allowedProviders === null;

/**
 * Whether an endpoint satisfies a policy. Only recorded terms count: an
 * endpoint with nothing recorded has no region, and its retention and
 * training are the worst case.
 */
export const satisfies = (entry: ProviderCatalogEntry, policy: DataPolicy) =>
  entry.isInGateway &&
  (!policy.requireZeroRetention || entry.isZeroRetention) &&
  (!policy.forbidTraining || entry.isNeverTrainedOn) &&
  (policy.allowedRegions === null ||
    (entry.terms !== null &&
      policy.allowedRegions.includes(entry.terms.region))) &&
  (policy.allowedProviders === null ||
    policy.allowedProviders.includes(entry.provider));

/**
 * What the organization's keys may reach in the gateway. An open policy
 * reaches everything, including provider keys added later. Any other policy
 * names its endpoints, and none matching reaches nothing.
 */
export const accessFor = (
  policy: DataPolicy,
  catalog: ProviderCatalogEntry[],
): VirtualKeyAccess => {
  if (isOpen(policy)) {
    return { allowAll: true };
  }
  const keyIds = new Map<string, string[]>();
  for (const entry of catalog) {
    if (satisfies(entry, policy)) {
      keyIds.set(entry.provider, [
        ...(keyIds.get(entry.provider) ?? []),
        entry.keyId,
      ]);
    }
  }
  return {
    allowAll: false,
    providers: [...keyIds].map(([provider, ids]) => ({
      provider,
      keyIds: ids,
    })),
  };
};
