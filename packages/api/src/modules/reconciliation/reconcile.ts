import { and, desc, eq, gte, lt, sql } from "@workspace/db";
import {
  creditLedgerEntry,
  gatewayCustomer,
  reconciliationRun,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { getGateway } from "@workspace/gateway-client/server";
import { logger } from "@workspace/shared/logger";

import type { GatewayClient } from "@workspace/gateway-client/server";

/**
 * The daily check that the gateway and the ledger agree (step 10.1 in
 * docs/platform/PLAN.md). For one UTC day it compares, per organization, the
 * provider cost the gateway logged with the provider cost the ledger
 * recorded on each usage entry, cost_micro_usd: both in USD and before markup
 * (ADR 0005), so neither the USD to SAR rate nor its changes can make them
 * differ (ADR 0017). It also lists charges lost because a
 * hold was reaped without a settlement. A request that starts before
 * midnight and settles after it is logged on one day and charged on the
 * next, so a small difference on a busy organization can be that.
 */

const DAY_MS = 86_400_000;
/** Differences under this are rounding: USD absolute, and relative to the larger side. */
const TOLERANCE_USD = 0.000001;
const TOLERANCE_RATIO = 0.0001;
/**
 * Each entry's cost_micro_usd is rounded half up to the micro-USD, so the
 * ledger's sum may be off the gateway's by up to half a micro-USD per entry.
 */
const ROUNDING_PER_ENTRY_USD = 0.0000005;

export interface OrganizationDifference {
  organizationId: string;
  gatewayCostUsd: number;
  ledgerCostUsd: number;
  differenceUsd: number;
}

/** micro-USD to USD. */
const MICRO_USD = 1_000_000;

export interface ReapedHolds {
  organizationId: string;
  holds: number;
  heldMicroSar: number;
}

export interface ReconciliationFindings {
  organizations: OrganizationDifference[];
  /** Gateway cost no organization owns: "unassigned", or a customer we do not map. */
  unattributed: { customerId: string; costUsd: number }[];
  reapedHolds: ReapedHolds[];
  checkedOrganizations: number;
  error?: string;
}

export type ReconciliationStatus = "ok" | "mismatch" | "error";

/** The UTC day before `now`. */
export const previousDay = (now: Date) => {
  const to = new Date(
    Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()),
  );
  return { from: new Date(to.getTime() - DAY_MS), to };
};

export const reconcile = async (
  { from, to }: { from: Date; to: Date },
  gateway: GatewayClient = getGateway(),
): Promise<{
  id: string;
  status: ReconciliationStatus;
  findings: ReconciliationFindings;
}> => {
  let findings: ReconciliationFindings;
  let status: ReconciliationStatus;
  try {
    findings = await compare({ from, to }, gateway);
    status =
      findings.organizations.length > 0 ||
      findings.unattributed.length > 0 ||
      findings.reapedHolds.length > 0
        ? "mismatch"
        : "ok";
  } catch (error) {
    logger.error(error, "reconciliation could not read its sources");
    findings = {
      organizations: [],
      unattributed: [],
      reapedHolds: [],
      checkedOrganizations: 0,
      error: error instanceof Error ? error.message : String(error),
    };
    status = "error";
  }

  const [run] = await db
    .insert(reconciliationRun)
    .values({
      periodStart: from,
      periodEnd: to,
      status,
      findings: { ...findings },
    })
    .returning({ id: reconciliationRun.id });
  if (!run) {
    throw new Error("reconciliation_run insert returned no row");
  }
  return { id: run.id, status, findings };
};

const compare = async (
  { from, to }: { from: Date; to: Date },
  gateway: GatewayClient,
): Promise<ReconciliationFindings> => {
  const inWindow = and(
    gte(creditLedgerEntry.createdAt, from),
    lt(creditLedgerEntry.createdAt, to),
  );
  const [gatewayCosts, customers, ledgerCosts, reaped] = await Promise.all([
    gateway.getCustomerCosts(from, to),
    db
      .select({
        organizationId: gatewayCustomer.organizationId,
        customerId: gatewayCustomer.customerId,
      })
      .from(gatewayCustomer),
    db
      .select({
        organizationId: creditLedgerEntry.organizationId,
        costMicroUsd: sql<string>`coalesce(sum(${creditLedgerEntry.costMicroUsd}), 0)`,
        entries: sql<string>`count(*)`,
      })
      .from(creditLedgerEntry)
      .where(and(eq(creditLedgerEntry.kind, "usage"), inWindow))
      .groupBy(creditLedgerEntry.organizationId),
    // a hold closed after its expiry was released by the reaper; with no usage
    // entry for it, the attempt's charge never reached the ledger
    db.execute<{ organization_id: string; holds: string; held: string }>(sql`
      SELECT r.organization_id, count(*) AS holds, sum(r.amount) AS held
        FROM credit_reservation r
       WHERE r.settled_at >= ${from.toISOString()}::timestamp
         AND r.settled_at < ${to.toISOString()}::timestamp
         AND r.settled_at >= r.expires_at
         AND NOT EXISTS (
           SELECT 1 FROM credit_ledger_entry e WHERE e.idempotency_key = 'usage:' || r.id
         )
       GROUP BY r.organization_id`),
  ]);

  const organizationOf = new Map(
    customers.map((c) => [c.customerId, c.organizationId]),
  );
  const gatewayByOrganization = new Map<string, number>();
  const unattributed: ReconciliationFindings["unattributed"] = [];
  for (const row of gatewayCosts) {
    const organizationId = organizationOf.get(row.customerId);
    if (organizationId) {
      gatewayByOrganization.set(organizationId, row.costUsd);
    } else if (row.costUsd > TOLERANCE_USD) {
      unattributed.push({ customerId: row.customerId, costUsd: row.costUsd });
    }
  }
  const ledgerByOrganization = new Map(
    ledgerCosts.map((row) => [
      row.organizationId,
      {
        costUsd: Number(row.costMicroUsd) / MICRO_USD,
        entries: Number(row.entries),
      },
    ]),
  );

  const organizationIds = new Set([
    ...gatewayByOrganization.keys(),
    ...ledgerByOrganization.keys(),
  ]);
  const organizations: OrganizationDifference[] = [];
  for (const organizationId of organizationIds) {
    const gatewayCostUsd = gatewayByOrganization.get(organizationId) ?? 0;
    const ledger = ledgerByOrganization.get(organizationId);
    const ledgerCostUsd = ledger?.costUsd ?? 0;
    const differenceUsd = gatewayCostUsd - ledgerCostUsd;
    const allowed = Math.max(
      TOLERANCE_USD,
      ROUNDING_PER_ENTRY_USD * (ledger?.entries ?? 0),
      TOLERANCE_RATIO * Math.max(gatewayCostUsd, ledgerCostUsd),
    );
    if (Math.abs(differenceUsd) > allowed) {
      organizations.push({
        organizationId,
        gatewayCostUsd,
        ledgerCostUsd,
        differenceUsd,
      });
    }
  }
  organizations.sort(
    (a, b) => Math.abs(b.differenceUsd) - Math.abs(a.differenceUsd),
  );

  return {
    organizations,
    unattributed,
    reapedHolds: [...reaped].map((row) => ({
      organizationId: row.organization_id,
      holds: Number(row.holds),
      heldMicroSar: Number(row.held),
    })),
    checkedOrganizations: organizationIds.size,
  };
};

export const listReconciliationRuns = async (limit = 30) =>
  db
    .select()
    .from(reconciliationRun)
    .orderBy(
      desc(reconciliationRun.periodStart),
      desc(reconciliationRun.createdAt),
    )
    .limit(limit);
