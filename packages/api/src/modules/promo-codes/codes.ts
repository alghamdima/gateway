import { and, desc, eq, ilike, inArray, sql } from "@workspace/db";
import {
  creditLedgerEntry,
  creditLot,
  organization,
  promoCode,
  promoCodeChange,
  promoCodeOrganization,
  promoCodeRedemption,
  user,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import { promoCodeStatus } from "./math";

import type { LedgerTransaction } from "../credits/ledger";
import type { PromoCodeStatus } from "./math";
import type { SavePromoCodeInput } from "./schema";
import type { SelectPromoCode } from "@workspace/db/schema";

/**
 * Promo codes as staff manage them (BILL-08, ADMIN-21). Every change is
 * written to promo_code_change with the values before and after, in the same
 * transaction, and the admin route adds it to the audit log. Codes are never
 * deleted: the ledger and the redemptions refer to them.
 */

const MAX_LISTED = 500;
const REPORT_REDEMPTIONS = 100;

export interface PromoCodeListItem extends SelectPromoCode {
  status: PromoCodeStatus;
}

export const listPromoCodes = async ({
  q,
  status,
  at = new Date(),
}: {
  q?: string;
  status?: PromoCodeStatus;
  at?: Date;
}): Promise<PromoCodeListItem[]> => {
  const rows = await db
    .select()
    .from(promoCode)
    .where(q ? ilike(promoCode.code, `%${escapeLike(q)}%`) : undefined)
    .orderBy(desc(promoCode.createdAt))
    .limit(MAX_LISTED);
  return rows
    .map((row) => ({ ...row, status: promoCodeStatus(row, at) }))
    .filter((row) => !status || row.status === status);
};

const escapeLike = (value: string) => value.replace(/[\\%_]/g, "\\$&");

export interface PromoCodeDetail extends PromoCodeListItem {
  organizations: { id: string; name: string; slug: string }[];
}

export const getPromoCode = async (
  id: string,
  at: Date = new Date(),
): Promise<PromoCodeDetail> => {
  const [row] = await db.select().from(promoCode).where(eq(promoCode.id, id));
  if (!row) {
    throw new HttpException(HttpStatusCode.NOT_FOUND, {
      code: "error.notFound",
    });
  }
  const organizations = await db
    .select({
      id: organization.id,
      name: organization.name,
      slug: organization.slug,
    })
    .from(promoCodeOrganization)
    .innerJoin(
      organization,
      eq(organization.id, promoCodeOrganization.organizationId),
    )
    .where(eq(promoCodeOrganization.promoCodeId, id))
    .orderBy(organization.name);
  return { ...row, status: promoCodeStatus(row, at), organizations };
};

/** The values a change records, as JSON: the audit log and promo_code_change keep the same. */
export interface PromoCodeValues extends Record<string, unknown> {
  code: string;
  kind: string;
  feeDiscountBps: number | null;
  feeDiscountMicroSar: number | null;
  bonusBps: number | null;
  bonusCapMicroSar: number | null;
  creditMicroSar: number | null;
  maxUses: number | null;
  onePerOrganization: boolean;
  startsAt: string | null;
  endsAt: string | null;
  /** When the credit it grants expires (BILL-17); null: the purchase rule. */
  creditExpiresAt: string | null;
  minTopupMicroSar: number | null;
  allOrganizations: boolean;
  organizationIds: string[];
  isPaused: boolean;
  note: string;
}

export interface PromoCodeChangeResult {
  code: SelectPromoCode;
  before: PromoCodeValues | null;
  after: PromoCodeValues;
}

const toValues = (
  row: SelectPromoCode,
  organizationIds: string[],
): PromoCodeValues => ({
  code: row.code,
  kind: row.kind,
  feeDiscountBps: row.feeDiscountBps,
  feeDiscountMicroSar: row.feeDiscountMicroSar,
  bonusBps: row.bonusBps,
  bonusCapMicroSar: row.bonusCapMicroSar,
  creditMicroSar: row.creditMicroSar,
  maxUses: row.maxUses,
  onePerOrganization: row.onePerOrganization,
  startsAt: row.startsAt?.toISOString() ?? null,
  endsAt: row.endsAt?.toISOString() ?? null,
  creditExpiresAt: row.creditExpiresAt?.toISOString() ?? null,
  minTopupMicroSar: row.minTopupMicroSar,
  allOrganizations: row.allOrganizations,
  organizationIds: [...organizationIds].sort(),
  isPaused: row.isPaused,
  note: row.note,
});

/** The stored columns of a save; the list of organizations is kept only when the code is not for all. */
const toColumns = (input: SavePromoCodeInput) => ({
  code: input.code,
  kind: input.kind,
  feeDiscountBps: input.feeDiscountBps,
  feeDiscountMicroSar: input.feeDiscountMicroSar,
  bonusBps: input.bonusBps,
  bonusCapMicroSar: input.bonusCapMicroSar,
  creditMicroSar: input.creditMicroSar,
  maxUses: input.maxUses,
  onePerOrganization: input.onePerOrganization,
  startsAt: input.startsAt,
  endsAt: input.endsAt,
  creditExpiresAt: input.creditExpiresAt,
  minTopupMicroSar: input.minTopupMicroSar,
  allOrganizations: input.allOrganizations,
  note: input.note,
});

const organizationIdsOf = (input: SavePromoCodeInput) =>
  input.allOrganizations ? [] : [...new Set(input.organizationIds)];

const listedOrganizations = async (tx: LedgerTransaction, codeId: string) =>
  (
    await tx
      .select({ id: promoCodeOrganization.organizationId })
      .from(promoCodeOrganization)
      .where(eq(promoCodeOrganization.promoCodeId, codeId))
  ).map((row) => row.id);

const replaceOrganizations = async (
  tx: LedgerTransaction,
  codeId: string,
  organizationIds: string[],
) => {
  await tx
    .delete(promoCodeOrganization)
    .where(eq(promoCodeOrganization.promoCodeId, codeId));
  if (organizationIds.length === 0) {
    return;
  }
  const found = await tx
    .select({ id: organization.id })
    .from(organization)
    .where(inArray(organization.id, organizationIds));
  if (found.length !== organizationIds.length) {
    throw new HttpException(HttpStatusCode.BAD_REQUEST, {
      message: "One of the chosen organizations does not exist",
    });
  }
  await tx.insert(promoCodeOrganization).values(
    organizationIds.map((organizationId) => ({
      promoCodeId: codeId,
      organizationId,
    })),
  );
};

const codeTaken = () =>
  new HttpException(HttpStatusCode.CONFLICT, {
    message: "Another promo code already uses this code",
  });

export const createPromoCode = async (
  input: SavePromoCodeInput & { staffId: string },
): Promise<PromoCodeChangeResult> =>
  db.transaction(async (tx) => {
    const [taken] = await tx
      .select({ id: promoCode.id })
      .from(promoCode)
      .where(eq(promoCode.code, input.code));
    if (taken) {
      throw codeTaken();
    }
    const [created] = await tx
      .insert(promoCode)
      .values({
        ...toColumns(input),
        createdBy: input.staffId,
        updatedBy: input.staffId,
      })
      .onConflictDoNothing({ target: promoCode.code })
      .returning();
    if (!created) {
      throw codeTaken();
    }
    const organizationIds = organizationIdsOf(input);
    await replaceOrganizations(tx, created.id, organizationIds);
    const after = toValues(created, organizationIds);
    await tx.insert(promoCodeChange).values({
      promoCodeId: created.id,
      before: null,
      after,
      changedBy: input.staffId,
    });
    return { code: created, before: null, after };
  });

/**
 * Edits a code. The row is locked, so two staff saving at once are recorded
 * in order and a redemption in progress finishes on the terms it read. Once
 * a code has been used its kind cannot change: its redemptions and ledger
 * entries say what kind of code granted them.
 */
export const updatePromoCode = async (
  input: SavePromoCodeInput & { id: string; staffId: string },
): Promise<PromoCodeChangeResult> =>
  db.transaction(async (tx) => {
    const current = await lockCode(tx, input.id);
    if (current.kind !== input.kind && (await hasRedemptions(tx, input.id))) {
      throw new HttpException(HttpStatusCode.CONFLICT, {
        message: "This code has been used: its kind cannot change",
      });
    }
    if (current.code !== input.code) {
      const [taken] = await tx
        .select({ id: promoCode.id })
        .from(promoCode)
        .where(eq(promoCode.code, input.code));
      if (taken) {
        throw codeTaken();
      }
    }
    const before = toValues(current, await listedOrganizations(tx, input.id));
    const [updated] = await tx
      .update(promoCode)
      .set({
        ...toColumns(input),
        updatedAt: new Date(),
        updatedBy: input.staffId,
      })
      .where(eq(promoCode.id, input.id))
      .returning();
    if (!updated) {
      throw new Error("promo_code update returned no row");
    }
    const organizationIds = organizationIdsOf(input);
    await replaceOrganizations(tx, input.id, organizationIds);
    const after = toValues(updated, organizationIds);
    await tx.insert(promoCodeChange).values({
      promoCodeId: input.id,
      before,
      after,
      changedBy: input.staffId,
    });
    return { code: updated, before, after };
  });

/** Pauses or resumes a code. Setting what is already set changes and records nothing. */
export const setPromoCodePaused = async ({
  id,
  isPaused,
  staffId,
}: {
  id: string;
  isPaused: boolean;
  staffId: string;
}): Promise<PromoCodeChangeResult> =>
  db.transaction(async (tx) => {
    const current = await lockCode(tx, id);
    const organizationIds = await listedOrganizations(tx, id);
    const before = toValues(current, organizationIds);
    if (current.isPaused === isPaused) {
      return { code: current, before, after: before };
    }
    const [updated] = await tx
      .update(promoCode)
      .set({ isPaused, updatedAt: new Date(), updatedBy: staffId })
      .where(eq(promoCode.id, id))
      .returning();
    if (!updated) {
      throw new Error("promo_code update returned no row");
    }
    const after = toValues(updated, organizationIds);
    await tx
      .insert(promoCodeChange)
      .values({ promoCodeId: id, before, after, changedBy: staffId });
    return { code: updated, before, after };
  });

const lockCode = async (tx: LedgerTransaction, id: string) => {
  const [row] = await tx
    .select()
    .from(promoCode)
    .where(eq(promoCode.id, id))
    .for("update");
  if (!row) {
    throw new HttpException(HttpStatusCode.NOT_FOUND, {
      code: "error.notFound",
    });
  }
  return row;
};

const hasRedemptions = async (tx: LedgerTransaction, id: string) =>
  (
    await tx
      .select({ id: promoCodeRedemption.id })
      .from(promoCodeRedemption)
      .where(eq(promoCodeRedemption.promoCodeId, id))
      .limit(1)
  ).length > 0;

/** The code's changes, newest first, with who made each one. */
export const getPromoCodeChanges = async (id: string, limit = 50) =>
  db
    .select({
      id: promoCodeChange.id,
      before: promoCodeChange.before,
      after: promoCodeChange.after,
      changedAt: promoCodeChange.changedAt,
      changedBy: promoCodeChange.changedBy,
      changedByName: user.name,
      changedByEmail: user.email,
    })
    .from(promoCodeChange)
    .leftJoin(user, eq(user.id, promoCodeChange.changedBy))
    .where(eq(promoCodeChange.promoCodeId, id))
    .orderBy(desc(promoCodeChange.id))
    .limit(limit);

export interface PromoCodeReport {
  /** Recorded redemptions. */
  uses: number;
  /** Distinct organizations that redeemed it. */
  organizations: number;
  /** Bonus and free credit: the sum of the code's promo ledger entries. */
  creditGrantedMicroSar: number;
  /**
   * Bonus taken back when its top-up was refunded: minus the sum of the
   * code's promo_clawback entries (ADR 0019). Granted less this is what the
   * code left with organizations.
   */
  creditClawedBackMicroSar: number;
  /** Taken off top-up fees: the sum the code's top-up entries record. */
  feeWaivedMicroSar: number;
  redemptions: {
    id: string;
    organizationId: string;
    organizationName: string | null;
    paymentId: string | null;
    topupCreditMicroSar: number | null;
    feeDiscountMicroSar: number;
    creditMicroSar: number;
    /** What refunds of its top-up took back of this use's bonus. */
    clawedBackMicroSar: number;
    createdAt: Date;
  }[];
}

/**
 * The report on one code. The money is read from the ledger, the source of
 * truth: credit granted is the sum of its promo entries, clawed back minus
 * the sum of its promo_clawback entries, fee waived the sum of the discounts
 * its top-up entries record (a refund never returns the fee, so a refund
 * leaves it as it was). Uses and organizations are counted from the redemptions.
 */
export const getPromoCodeReport = async (
  id: string,
): Promise<PromoCodeReport> => {
  await getPromoCode(id);
  const [counts, granted, clawedBack, waived, redemptions] = await Promise.all([
    db
      .select({
        uses: sql<string>`count(*)`,
        organizations: sql<string>`count(distinct ${promoCodeRedemption.organizationId})`,
      })
      .from(promoCodeRedemption)
      .where(eq(promoCodeRedemption.promoCodeId, id)),
    db
      .select({
        total: sql<string>`coalesce(sum(${creditLedgerEntry.amount}), 0)`,
      })
      .from(creditLedgerEntry)
      .where(
        and(
          eq(creditLedgerEntry.promoCodeId, id),
          eq(creditLedgerEntry.kind, "promo"),
        ),
      ),
    db
      .select({
        total: sql<string>`coalesce(-sum(${creditLedgerEntry.amount}), 0)`,
      })
      .from(creditLedgerEntry)
      .where(
        and(
          eq(creditLedgerEntry.promoCodeId, id),
          eq(creditLedgerEntry.kind, "promo_clawback"),
        ),
      ),
    db
      .select({
        total: sql<string>`coalesce(sum((${creditLedgerEntry.metadata} ->> 'promoFeeDiscountMicroSar')::bigint), 0)`,
      })
      .from(creditLedgerEntry)
      .where(
        and(
          eq(creditLedgerEntry.promoCodeId, id),
          eq(creditLedgerEntry.kind, "top_up"),
        ),
      ),
    db
      .select({
        id: promoCodeRedemption.id,
        organizationId: promoCodeRedemption.organizationId,
        organizationName: organization.name,
        paymentId: promoCodeRedemption.paymentId,
        topupCreditMicroSar: promoCodeRedemption.topupCreditMicroSar,
        feeDiscountMicroSar: promoCodeRedemption.feeDiscountMicroSar,
        creditMicroSar: promoCodeRedemption.creditMicroSar,
        // the clawbacks name the lot of this use's bonus entry
        clawedBackMicroSar: sql<number>`coalesce((
          SELECT -sum(c.amount) FROM ${creditLedgerEntry} c
            JOIN ${creditLot} l ON l.id = c.credit_lot_id
           WHERE l.entry_id = ${promoCodeRedemption.ledgerEntryId}
             AND c.kind = 'promo_clawback'), 0)`.mapWith(Number),
        createdAt: promoCodeRedemption.createdAt,
      })
      .from(promoCodeRedemption)
      .leftJoin(
        organization,
        eq(organization.id, promoCodeRedemption.organizationId),
      )
      .where(eq(promoCodeRedemption.promoCodeId, id))
      .orderBy(desc(promoCodeRedemption.createdAt))
      .limit(REPORT_REDEMPTIONS),
  ]);
  return {
    uses: Number(counts[0]?.uses ?? 0),
    organizations: Number(counts[0]?.organizations ?? 0),
    creditGrantedMicroSar: Number(granted[0]?.total ?? 0),
    creditClawedBackMicroSar: Number(clawedBack[0]?.total ?? 0),
    feeWaivedMicroSar: Number(waived[0]?.total ?? 0),
    redemptions,
  };
};
