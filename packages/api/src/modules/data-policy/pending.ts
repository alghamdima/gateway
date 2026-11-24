import { eq, sql } from "@workspace/db";
import { organizationDataPolicy } from "@workspace/db/schema";
import { db } from "@workspace/db/server";

/**
 * Marks every policy as not applied (ADR 0015). Called in the same
 * transaction as a change to an endpoint's terms: what the keys carry may no
 * longer be what the policies imply, until applyPendingPolicies pushes them.
 */
export const markPoliciesUnapplied = (tx: Pick<typeof db, "update"> = db) =>
  tx
    .update(organizationDataPolicy)
    .set({ appliedVersion: sql`${organizationDataPolicy.version} - 1` })
    .where(
      eq(organizationDataPolicy.appliedVersion, organizationDataPolicy.version),
    );
