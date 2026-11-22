import { generateId } from "@workspace/shared/utils";

import { applyEntry } from "./ledger";

export interface AdjustCreditsInput {
  organizationId: string;
  /** Signed micro-USD. */
  amount: number;
  reason: string;
  /** The staff user who made the decision. */
  staffId: string;
  idempotencyKey?: string;
}

/** A manual correction by staff: recorded like any other entry, with who and why. */
export const adjustCredits = async ({
  organizationId,
  amount,
  reason,
  staffId,
  idempotencyKey,
}: AdjustCreditsInput) =>
  applyEntry({
    organizationId,
    amount,
    kind: "adjustment",
    idempotencyKey: `adjustment:${idempotencyKey ?? generateId()}`,
    reference: staffId,
    metadata: { reason, staffId },
  });
