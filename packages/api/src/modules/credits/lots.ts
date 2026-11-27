import { and, asc, eq, inArray, sql } from "@workspace/db";
import {
  creditLedgerEntry,
  creditLot,
  creditRefund,
  pricingSettings,
  promoCode,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { MICRO_SAR_PER_HALALA } from "@workspace/shared/utils";

import type { LedgerTransaction } from "./ledger";
import type {
  CreditEntryKind,
  CreditLotSource,
  CreditRefundStatus,
} from "@workspace/db/schema";

/**
 * Credit lots (BILL-17, BILL-10, docs/platform/adr/0019-credit-lots-expiry-refunds.md).
 *
 * Every grant of credit (a top-up, a promo code's credit, a positive staff
 * adjustment) is a lot with the expiry and refund window in force when it was
 * granted. How much of a lot is left is not stored: it is worked out from the
 * ledger by `attributeLots`, so the gateway's settlement stays one statement
 * that knows nothing of lots. The rule, in ledger order (created_at, then id):
 *
 * - a grant opens its lot; it first pays any debt (spending that no lot covered);
 * - a refund or expiry entry takes from the lot it names, up to what is left
 *   of it; any rest is spent like usage;
 * - usage, a negative adjustment and a refund that names no lot are spent from
 *   the open lots, the one that expires first first (never-expiring last); on
 *   the same expiry, promo credit before adjustments before purchased credit,
 *   then the older lot, then the lower id; what no lot covers is debt.
 *
 * So the lots' remainders always add up to the balance when it is positive,
 * and to zero when it is not (checkLotInvariants in reconciliation).
 */

/** Which lot is spent first on the same expiry date: free credit before bought credit. */
const SOURCE_ORDER: Record<CreditLotSource, number> = {
  promo: 0,
  adjustment: 1,
  top_up: 2,
};

export interface LotTerms {
  id: string;
  source: CreditLotSource;
  grantedMicroSar: number;
  expiresAt: Date | null;
  createdAt: Date;
}

/** One step of an organization's ledger, as the attribution needs it. Amounts are positive. */
export type LotEvent =
  | { type: "grant"; lot: LotTerms }
  | { type: "lot_debit"; lotId: string; amountMicroSar: number }
  | { type: "debit"; amountMicroSar: number };

export interface LotAttribution {
  /** What is left of each lot, by id, after every event. */
  remaining: Map<string, number>;
  /** Spending no lot covered (a negative balance). */
  debtMicroSar: number;
}

/** Earlier expiry first, never-expiring last; then free credit first, then older, then lower id. */
export const compareLots = (a: LotTerms, b: LotTerms) => {
  if (a.expiresAt?.getTime() !== b.expiresAt?.getTime()) {
    if (!a.expiresAt) {
      return 1;
    }
    if (!b.expiresAt) {
      return -1;
    }
    return a.expiresAt.getTime() - b.expiresAt.getTime();
  }
  if (a.source !== b.source) {
    return SOURCE_ORDER[a.source] - SOURCE_ORDER[b.source];
  }
  if (a.createdAt.getTime() !== b.createdAt.getTime()) {
    return a.createdAt.getTime() - b.createdAt.getTime();
  }
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
};

/** Replays the events in order and says what is left of each lot (the rule above). */
export const attributeLots = (events: LotEvent[]): LotAttribution => {
  const remaining = new Map<string, number>();
  const open: LotTerms[] = [];
  let debtMicroSar = 0;

  const spend = (amount: number) => {
    let left = amount;
    for (const lot of open) {
      if (left === 0) {
        break;
      }
      const have = remaining.get(lot.id) ?? 0;
      const taken = Math.min(have, left);
      remaining.set(lot.id, have - taken);
      left -= taken;
    }
    debtMicroSar += left;
  };

  for (const event of events) {
    if (event.type === "grant") {
      const paid = Math.min(debtMicroSar, event.lot.grantedMicroSar);
      debtMicroSar -= paid;
      remaining.set(event.lot.id, event.lot.grantedMicroSar - paid);
      open.push(event.lot);
      open.sort(compareLots);
    } else if (event.type === "lot_debit") {
      const have = remaining.get(event.lotId) ?? 0;
      const taken = Math.min(have, event.amountMicroSar);
      remaining.set(event.lotId, have - taken);
      spend(event.amountMicroSar - taken);
    } else {
      spend(event.amountMicroSar);
    }
  }
  return { remaining, debtMicroSar };
};

interface TimelineRow extends Record<string, unknown> {
  id: string | null;
  kind: string;
  amount: string;
  credit_lot_id: string | null;
  lot_id: string | null;
  source: CreditLotSource | null;
  granted_micro_sar: string | null;
  expires_at: string | null;
  lot_created_at: string | null;
}

/**
 * An organization's ledger as lot events, in ledger order. Usage entries
 * between two other entries are summed in Postgres (they all draw the same
 * way), so the rows read are the grants, refunds, adjustments and expiries
 * plus one sum per gap. Run it inside the transaction that holds the
 * account's lock when the answer decides a write.
 */
export const loadLotEvents = async (
  tx: LedgerTransaction | typeof db,
  organizationId: string,
): Promise<LotEvent[]> => {
  const rows = await tx.execute<TimelineRow>(sql`
    WITH s AS (
      SELECT e.id, e.kind::text AS kind, e.amount, e.credit_lot_id,
             count(*) FILTER (WHERE e.kind <> 'usage')
               OVER (ORDER BY e.created_at, e.id) AS bucket
        FROM ${creditLedgerEntry} e
       WHERE e.organization_id = ${organizationId}
    ), steps AS (
      SELECT s.bucket, 0 AS part, s.id, s.kind, s.amount, s.credit_lot_id,
             l.id AS lot_id, l.source, l.granted_micro_sar,
             to_char(l.expires_at, 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') AS expires_at,
             to_char(l.created_at, 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') AS lot_created_at
        FROM s LEFT JOIN ${creditLot} l ON l.entry_id = s.id
       WHERE s.kind <> 'usage'
      UNION ALL
      SELECT s.bucket, 1, NULL, 'usage', sum(s.amount), NULL,
             NULL, NULL, NULL, NULL, NULL
        FROM s WHERE s.kind = 'usage' GROUP BY s.bucket
    )
    SELECT id, kind, amount::text AS amount, credit_lot_id, lot_id, source,
           granted_micro_sar::text AS granted_micro_sar, expires_at, lot_created_at
      FROM steps ORDER BY bucket, part`);
  return [...rows].map(toLotEvent);
};

const toLotEvent = (row: TimelineRow): LotEvent => {
  const amount = Number(row.amount);
  if (row.lot_id && row.source && row.lot_created_at) {
    return {
      type: "grant",
      lot: {
        id: row.lot_id,
        source: row.source,
        grantedMicroSar: Number(row.granted_micro_sar),
        expiresAt: row.expires_at ? new Date(row.expires_at) : null,
        createdAt: new Date(row.lot_created_at),
      },
    };
  }
  if (amount > 0) {
    // a credit with no lot (none is written since migration 0038; the
    // reconciliation check lists any): it never expires and is spent last
    return {
      type: "grant",
      lot: {
        id: `entry:${row.id ?? ""}`,
        source: "adjustment",
        grantedMicroSar: amount,
        expiresAt: null,
        createdAt: new Date(0),
      },
    };
  }
  if (row.credit_lot_id) {
    return {
      type: "lot_debit",
      lotId: row.credit_lot_id,
      amountMicroSar: -amount,
    };
  }
  return { type: "debit", amountMicroSar: -amount };
};

/** What is left of each of the organization's lots now. */
export const getLotRemainders = async (
  tx: LedgerTransaction | typeof db,
  organizationId: string,
) => attributeLots(await loadLotEvents(tx, organizationId));

/** The lot a ledger entry opens, if its kind and sign grant credit. */
export const lotSourceOf = (
  kind: CreditEntryKind,
  amount: number,
): CreditLotSource | null => {
  if (kind === "top_up" || kind === "promo") {
    return kind;
  }
  return kind === "adjustment" && amount > 0 ? "adjustment" : null;
};

/**
 * Opens the lot of a grant just written, in the caller's transaction, with
 * the terms in force now (ADMIN-23): a top-up expires credit_expiry_days from
 * now and may be refunded for refund_window_hours (none when 0); promo credit
 * expires on its code's own date, else as a top-up (BILL-08 decisions); a
 * positive adjustment never expires (ADR 0019: it corrects the account).
 */
export const openLotIn = async (
  tx: LedgerTransaction,
  input: {
    entryId: string;
    organizationId: string;
    source: CreditLotSource;
    amount: number;
    promoCodeId?: string;
  },
) => {
  const purchaseExpiry = sql`now() + make_interval(days => ${pricingSettings.creditExpiryDays})`;
  const promoExpiry = input.promoCodeId
    ? sql`(SELECT ${promoCode.creditExpiresAt} FROM ${promoCode} WHERE ${promoCode.id} = ${input.promoCodeId})`
    : sql`NULL::timestamp`;
  const expiresAt =
    input.source === "adjustment"
      ? sql`NULL::timestamp`
      : input.source === "promo"
        ? sql`coalesce(${promoExpiry}, ${purchaseExpiry})`
        : purchaseExpiry;
  const refundableUntil =
    input.source === "top_up"
      ? sql`CASE WHEN ${pricingSettings.refundWindowHours} > 0
                 THEN now() + make_interval(hours => ${pricingSettings.refundWindowHours}) END`
      : sql`NULL::timestamp`;

  const [lot] = await tx.execute<{ id: string }>(sql`
    INSERT INTO ${creditLot} (id, organization_id, entry_id, source, granted_micro_sar, expires_at, refundable_until)
    SELECT gen_random_uuid()::text, ${input.organizationId}, ${input.entryId}, ${input.source},
           ${input.amount}, ${expiresAt}, ${refundableUntil}
      FROM ${pricingSettings} WHERE ${pricingSettings.id} = 1
    RETURNING id`);
  if (!lot) {
    throw new Error("pricing_settings has no row: run the database migrations");
  }
  return lot.id;
};

/** Down to a whole halala: what a provider can pay back. */
export const floorToHalala = (microSar: number) =>
  microSar - (microSar % MICRO_SAR_PER_HALALA);

/** How soon before an expiry the credits page warns (BILL-17). */
export const EXPIRY_NOTICE_DAYS = [30, 7] as const;
const DAY_MS = 86_400_000;

export type LotRefundState =
  /** The button: the refund window is open and part of the credit is unused. */
  | "available"
  | CreditRefundStatus
  /** Staff refunded (part of) the payment in the provider's dashboard. */
  | "refunded"
  /** Not a top-up, the window has passed, refunds are off, or nothing whole is left. */
  | "none";

export interface CreditLotView {
  id: string;
  source: CreditLotSource;
  grantedMicroSar: number;
  remainingMicroSar: number;
  createdAt: string;
  expiresAt: string | null;
  refund: {
    state: LotRefundState;
    /** What the refund returns (or returned): the unused credit in whole halalas. */
    amountMicroSar: number;
    refundableUntil: string | null;
  };
}

export interface CreditLotsView {
  /** Open lots (something left), the one spent first first. */
  lots: CreditLotView[];
  /** Whether staff have self-serve refunds on (ADMIN-23). */
  areRefundsEnabled: boolean;
  /** Credit that expires within 7 and within 30 days, and the first such date (BILL-17). */
  expiring: {
    withinDays: (typeof EXPIRY_NOTICE_DAYS)[number];
    amountMicroSar: number;
    firstExpiresAt: string;
  } | null;
}

/** The credits page: each lot's remainder, expiry and refund, and the expiry notice. */
export const getCreditLots = async (
  organizationId: string,
  now = new Date(),
): Promise<CreditLotsView> => {
  const [attribution, lots, refunds, lotRefundEntries, [settings]] =
    await Promise.all([
      getLotRemainders(db, organizationId),
      db
        .select()
        .from(creditLot)
        .where(eq(creditLot.organizationId, organizationId))
        .orderBy(asc(creditLot.createdAt)),
      db
        .select()
        .from(creditRefund)
        .where(eq(creditRefund.organizationId, organizationId)),
      db
        .select({ creditLotId: creditLedgerEntry.creditLotId })
        .from(creditLedgerEntry)
        .where(
          and(
            eq(creditLedgerEntry.organizationId, organizationId),
            eq(creditLedgerEntry.kind, "refund"),
          ),
        ),
      db
        .select({ refundWindowHours: pricingSettings.refundWindowHours })
        .from(pricingSettings)
        .where(eq(pricingSettings.id, 1)),
    ]);
  const areRefundsEnabled = (settings?.refundWindowHours ?? 0) > 0;
  const refundOf = new Map(refunds.map((row) => [row.creditLotId, row]));
  const refundedLots = new Set(lotRefundEntries.map((row) => row.creditLotId));

  const open = lots
    .map((lot) => ({
      lot,
      remainingMicroSar: attribution.remaining.get(lot.id) ?? 0,
    }))
    .filter(
      ({ lot, remainingMicroSar }) =>
        remainingMicroSar > 0 && lot.expiredAt === null,
    )
    .sort((a, b) => compareLots(a.lot, b.lot));

  const views = open.map(({ lot, remainingMicroSar }): CreditLotView => {
    const request = refundOf.get(lot.id);
    const amountMicroSar =
      request?.amountMicroSar ?? floorToHalala(remainingMicroSar);
    const state: LotRefundState = request
      ? request.status
      : refundedLots.has(lot.id)
        ? "refunded"
        : isRefundable({ lot, amountMicroSar, areRefundsEnabled, now })
          ? "available"
          : "none";
    return {
      id: lot.id,
      source: lot.source,
      grantedMicroSar: lot.grantedMicroSar,
      remainingMicroSar,
      createdAt: lot.createdAt.toISOString(),
      expiresAt: lot.expiresAt?.toISOString() ?? null,
      refund: {
        state,
        amountMicroSar,
        refundableUntil: lot.refundableUntil?.toISOString() ?? null,
      },
    };
  });

  return { lots: views, areRefundsEnabled, expiring: expiringSoon(views, now) };
};

const isRefundable = ({
  lot,
  amountMicroSar,
  areRefundsEnabled,
  now,
}: {
  lot: { source: CreditLotSource; refundableUntil: Date | null };
  amountMicroSar: number;
  areRefundsEnabled: boolean;
  now: Date;
}) =>
  areRefundsEnabled &&
  lot.source === "top_up" &&
  lot.refundableUntil !== null &&
  lot.refundableUntil.getTime() > now.getTime() &&
  amountMicroSar > 0;

/** The nearest notice that applies: 7 days before, else 30 days before an expiry. */
export const expiringSoon = (
  lots: Pick<CreditLotView, "remainingMicroSar" | "expiresAt">[],
  now: Date,
): CreditLotsView["expiring"] => {
  for (const withinDays of [...EXPIRY_NOTICE_DAYS].sort((a, b) => a - b)) {
    const until = now.getTime() + withinDays * DAY_MS;
    const soon = lots.filter(
      (lot) =>
        lot.expiresAt !== null &&
        new Date(lot.expiresAt).getTime() <= until &&
        lot.remainingMicroSar > 0,
    );
    if (soon.length > 0) {
      const first = soon
        .map((lot) => lot.expiresAt ?? "")
        .sort()
        .at(0);
      return {
        withinDays,
        amountMicroSar: soon.reduce(
          (sum, lot) => sum + lot.remainingMicroSar,
          0,
        ),
        firstExpiresAt: first ?? "",
      };
    }
  }
  return null;
};

/** The lots of these entries (grants), by entry id. */
export const lotsOfEntries = async (
  tx: LedgerTransaction | typeof db,
  entryIds: string[],
) =>
  entryIds.length === 0
    ? []
    : tx.select().from(creditLot).where(inArray(creditLot.entryId, entryIds));
