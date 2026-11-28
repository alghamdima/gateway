import { isNull, sql } from "@workspace/db";
import { creditLot } from "@workspace/db/schema";
import { db } from "@workspace/db/server";

import { getLotRemainders } from "../credits/lots";

/**
 * The ledger's own invariants (ADR 0001, ADR 0019), checked with the daily
 * reconciliation:
 *
 * - each account's balance is the sum of its ledger entries, and its held
 *   amount the sum of its open holds (the account row is a cache);
 * - every grant of credit has its lot;
 * - what is left of an organization's lots adds up to its balance when that
 *   is positive, and to zero otherwise (the attribution rule in credits/lots.ts);
 * - every credit transfer (ADR 0020) took from the personal account exactly
 *   what it gave the organization, which is the amount it records: a transfer
 *   creates no credit and loses none.
 *
 * A finding means the cache, a grant or the attribution disagrees with the
 * ledger, which is the source of truth: staff look at it, and
 * rebuildBalance repairs a cache.
 */

export interface BalanceFinding {
  organizationId: string;
  balance: number;
  ledgerSum: number;
}

export interface HeldFinding {
  organizationId: string;
  held: number;
  openHolds: number;
}

export interface LotFinding {
  organizationId: string;
  /** What the lots say is left, and the balance it must match (0 when the balance is negative). */
  lotsRemaining: number;
  expected: number;
}

export interface TransferFinding {
  transferId: string;
  /** What the transfer records, and what its entries took out and put in (micro-SAR, positive). */
  amountMicroSar: number;
  out: number;
  in: number;
}

export interface LedgerInvariantFindings {
  balances: BalanceFinding[];
  held: HeldFinding[];
  /** Grant entries with no lot. */
  grantsWithoutLot: { organizationId: string; entryId: string }[];
  lots: LotFinding[];
  /** Transfers whose two sides differ, and transfer entries that belong to no transfer. */
  transfers: TransferFinding[];
}

export const hasLedgerFindings = (findings: LedgerInvariantFindings) =>
  findings.balances.length > 0 ||
  findings.held.length > 0 ||
  findings.grantsWithoutLot.length > 0 ||
  findings.lots.length > 0 ||
  findings.transfers.length > 0;

export const checkLedgerInvariants =
  async (): Promise<LedgerInvariantFindings> => {
    const [balances, held, grantsWithoutLot, transfers, withLots] =
      await Promise.all([
        db.execute<{
          organization_id: string;
          balance: string;
          ledger: string;
        }>(sql`
        SELECT a.organization_id, a.balance::text AS balance, coalesce(e.total, 0)::text AS ledger
          FROM credit_account a
          LEFT JOIN (SELECT organization_id, sum(amount) AS total
                       FROM credit_ledger_entry GROUP BY organization_id) e
            ON e.organization_id = a.organization_id
         WHERE a.balance <> coalesce(e.total, 0)
        UNION ALL
        -- entries of an organization that has no account row at all
        SELECT e.organization_id, '0', sum(e.amount)::text
          FROM credit_ledger_entry e
         WHERE NOT EXISTS (SELECT 1 FROM credit_account a WHERE a.organization_id = e.organization_id)
         GROUP BY e.organization_id
        HAVING sum(e.amount) <> 0`),
        db.execute<{ organization_id: string; held: string; open: string }>(sql`
        SELECT a.organization_id, a.held::text AS held, coalesce(r.total, 0)::text AS open
          FROM credit_account a
          LEFT JOIN (SELECT organization_id, sum(amount) AS total
                       FROM credit_reservation WHERE settled_at IS NULL GROUP BY organization_id) r
            ON r.organization_id = a.organization_id
         WHERE a.held <> coalesce(r.total, 0)`),
        db.execute<{ organization_id: string; id: string }>(sql`
        SELECT e.organization_id, e.id
          FROM credit_ledger_entry e
         WHERE (e.kind::text IN ('top_up', 'promo', 'transfer_in') OR (e.kind = 'adjustment' AND e.amount > 0))
           AND NOT EXISTS (SELECT 1 FROM credit_lot l WHERE l.entry_id = e.id)`),
        db.execute<{
          transfer_id: string;
          amount: string;
          out: string;
          in: string;
        }>(sql`
        SELECT coalesce(t.id, e.reference) AS transfer_id,
               coalesce(t.amount_micro_sar, 0)::text AS amount,
               coalesce(e.out, 0)::text AS out, coalesce(e."in", 0)::text AS "in"
          FROM credit_transfer t
          FULL JOIN (SELECT reference,
                            -sum(amount) FILTER (WHERE kind::text = 'transfer_out') AS out,
                            sum(amount) FILTER (WHERE kind::text = 'transfer_in') AS "in"
                       FROM credit_ledger_entry
                      WHERE kind::text IN ('transfer_out', 'transfer_in')
                      GROUP BY reference) e
            ON e.reference = t.id
         WHERE t.id IS NULL
            OR coalesce(e.out, 0) <> t.amount_micro_sar
            OR coalesce(e."in", 0) <> t.amount_micro_sar`),
        // only organizations with an open lot can disagree: a closed lot has nothing left
        db
          .selectDistinct({ organizationId: creditLot.organizationId })
          .from(creditLot)
          .where(isNull(creditLot.expiredAt)),
      ]);

    const lots: LotFinding[] = [];
    for (const { organizationId } of withLots) {
      // one snapshot for both reads, so a charge landing between them cannot look like a finding
      const { balance, remaining } = await db.transaction(
        async (tx) => {
          const [sum] = await tx.execute<{ balance: string }>(sql`
            SELECT coalesce(sum(amount), 0)::text AS balance
              FROM credit_ledger_entry WHERE organization_id = ${organizationId}`);
          return {
            balance: Number(sum?.balance ?? 0),
            ...(await getLotRemainders(tx, organizationId)),
          };
        },
        { isolationLevel: "repeatable read", accessMode: "read only" },
      );
      const lotsRemaining = [...remaining.values()].reduce((a, b) => a + b, 0);
      const expected = Math.max(balance, 0);
      if (lotsRemaining !== expected) {
        lots.push({ organizationId, lotsRemaining, expected });
      }
    }

    return {
      balances: [...balances].map((row) => ({
        organizationId: row.organization_id,
        balance: Number(row.balance),
        ledgerSum: Number(row.ledger),
      })),
      held: [...held].map((row) => ({
        organizationId: row.organization_id,
        held: Number(row.held),
        openHolds: Number(row.open),
      })),
      grantsWithoutLot: [...grantsWithoutLot].map((row) => ({
        organizationId: row.organization_id,
        entryId: row.id,
      })),
      lots,
      transfers: [...transfers].map((row) => ({
        transferId: row.transfer_id,
        amountMicroSar: Number(row.amount),
        out: Number(row.out),
        in: Number(row.in),
      })),
    };
  };
