import { and, asc, eq, inArray, isNull, sql } from "@workspace/db";
import {
  accountSettings,
  creditAccount,
  creditLot,
  creditRefund,
  creditTransfer,
  member,
  organization,
  personalAccount,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { HttpStatusCode } from "@workspace/shared/constants";
import { HttpException } from "@workspace/shared/utils";

import { applyEntryIn } from "./ledger";
import { compareLots, floorToHalala, getLotRemainders } from "./lots";

import type { LedgerTransaction } from "./ledger";

/**
 * Moving credit from a user's personal account to an organization they
 * belong to (BILL-12, docs/platform/adr/0020-personal-accounts.md).
 *
 * The gateway knows nothing of this: its credits plugin reads the cached
 * balance and writes usage entries only (apps/gateway/plugins/credits/store.go),
 * so a transfer is control-plane work on the ledger, as refunds and expiry are.
 *
 * The rules (owner, 2026-11-28):
 * - one way: personal to organization, never back and never between organizations;
 * - any member may fund their organization;
 * - bought credit only: what is left of the personal account's top-up lots.
 *   Promo credit and staff adjustments stay;
 * - moved credit keeps the expiry of the lot it left and is never refundable;
 * - two waits, as OpenRouter has: the user must have been a member for
 *   transfer_member_tenure_hours, and an organization that received a
 *   transfer waits transfer_cooldown_hours for the next (staff settings);
 * - the caller has checked a fresh two-factor code (the route).
 *
 * One transaction, under both accounts' locks (taken in id order, as the
 * gateway's settlement takes one): for each lot the credit leaves, earliest
 * expiry first, a `transfer_out` entry naming the lot and a `transfer_in`
 * entry of the same amount that opens a lot with the same expiry. Credit held
 * by requests in flight or by a pending refund is not moved.
 */

/** Why a transfer is refused; each is billing:error.transfer.<reason>. */
export const TRANSFER_REFUSALS = [
  "noPersonalAccount",
  "notAnOrganization",
  "notMember",
  "memberTooNew",
  "organizationCoolingDown",
  "insufficientCredit",
  "keyReused",
] as const;
export type TransferRefusal = (typeof TRANSFER_REFUSALS)[number];

const STATUS_OF: Record<TransferRefusal, HttpStatusCode> = {
  noPersonalAccount: HttpStatusCode.CONFLICT,
  notAnOrganization: HttpStatusCode.CONFLICT,
  notMember: HttpStatusCode.FORBIDDEN,
  memberTooNew: HttpStatusCode.CONFLICT,
  organizationCoolingDown: HttpStatusCode.CONFLICT,
  insufficientCredit: HttpStatusCode.CONFLICT,
  keyReused: HttpStatusCode.CONFLICT,
};

export const transferRefusal = (reason: TransferRefusal) =>
  new HttpException(STATUS_OF[reason], {
    code: `billing:error.transfer.${reason}`,
  });

/** Refused before the ledger is touched, by the route: billing:error.transfer.<reason> too. */
export const TRANSFER_GATE_REFUSALS = {
  twoFactorRequired: HttpStatusCode.FORBIDDEN,
  invalidCode: HttpStatusCode.FORBIDDEN,
  tooManyAttempts: HttpStatusCode.TOO_MANY_REQUESTS,
} as const;

export const transferGateRefusal = (
  reason: keyof typeof TRANSFER_GATE_REFUSALS,
) =>
  new HttpException(TRANSFER_GATE_REFUSALS[reason], {
    code: `billing:error.transfer.${reason}`,
  });

const ATTEMPT_WINDOW_MS = 60_000;
const MAX_ATTEMPTS_PER_WINDOW = 5;

/**
 * Counts one transfer attempt of the user and refuses the sixth within a
 * minute: each attempt checks a six-digit code, and Better Auth's own limit
 * on code checks guards its HTTP route, not a check made from here. Counted
 * in rate_limit, the table Better Auth's limits use, so every instance of
 * the web app shares the count.
 */
export const countTransferAttempt = async (
  userId: string,
  now = Date.now(),
) => {
  const [row] = await db.execute<{ count: number }>(sql`
    INSERT INTO rate_limit (id, key, count, last_request)
    VALUES (gen_random_uuid()::text, ${`credit-transfer:${userId}`}, 1, ${now})
    ON CONFLICT (key) DO UPDATE SET
      count = CASE WHEN rate_limit.last_request < ${now - ATTEMPT_WINDOW_MS} THEN 1 ELSE rate_limit.count + 1 END,
      last_request = CASE WHEN rate_limit.last_request < ${now - ATTEMPT_WINDOW_MS} THEN ${now} ELSE rate_limit.last_request END
    RETURNING count`);
  if ((row?.count ?? 0) > MAX_ATTEMPTS_PER_WINDOW) {
    throw transferGateRefusal("tooManyAttempts");
  }
};

const HOUR_MS = 3_600_000;

interface TransferWaits {
  transferMemberTenureHours: number;
  transferCooldownHours: number;
}

const getWaits = async (tx: LedgerTransaction | typeof db) => {
  const [waits] = await tx
    .select({
      transferMemberTenureHours: accountSettings.transferMemberTenureHours,
      transferCooldownHours: accountSettings.transferCooldownHours,
    })
    .from(accountSettings);
  if (!waits) {
    // the migration creates the row and nothing deletes it
    throw new Error("account_settings has no row: run the database migrations");
  }
  return waits;
};

/**
 * The top-up lots credit can leave, the one that expires first first, with
 * what is left of each: open, not past their expiry, and not waiting on a
 * refund. Run inside the transaction that holds the account's lock when the
 * answer decides a write.
 */
const transferableLots = async (
  tx: LedgerTransaction | typeof db,
  personalOrganizationId: string,
) => {
  const [{ remaining }, lots, refunds] = await Promise.all([
    getLotRemainders(tx, personalOrganizationId),
    tx
      .select()
      .from(creditLot)
      .where(
        and(
          eq(creditLot.organizationId, personalOrganizationId),
          eq(creditLot.source, "top_up"),
          isNull(creditLot.expiredAt),
          sql`(${creditLot.expiresAt} is null or ${creditLot.expiresAt} > now())`,
        ),
      )
      .orderBy(asc(creditLot.createdAt)),
    tx
      .select({ creditLotId: creditRefund.creditLotId })
      .from(creditRefund)
      .where(
        and(
          eq(creditRefund.organizationId, personalOrganizationId),
          eq(creditRefund.status, "pending"),
        ),
      ),
  ]);
  const refunding = new Set(refunds.map((refund) => refund.creditLotId));
  return lots
    .filter((lot) => !refunding.has(lot.id))
    .map((lot) => ({ lot, remainingMicroSar: remaining.get(lot.id) ?? 0 }))
    .filter(({ remainingMicroSar }) => remainingMicroSar > 0)
    .sort((a, b) => compareLots(a.lot, b.lot));
};

/** Bought credit that may leave now: never more than what no request or refund is holding. */
const transferableOf = (
  lots: { remainingMicroSar: number }[],
  account: { balance: number; held: number } | undefined,
) =>
  floorToHalala(
    Math.max(
      0,
      Math.min(
        lots.reduce((sum, lot) => sum + lot.remainingMicroSar, 0),
        (account?.balance ?? 0) - (account?.held ?? 0),
      ),
    ),
  );

export interface TransferDestination {
  id: string;
  name: string;
  slug: string;
  /** When this organization can receive the user's credit; null: now. */
  availableAt: string | null;
  /** Which wait `availableAt` is the end of. */
  waitingFor: "memberTenure" | "cooldown" | null;
}

export interface TransferOptions {
  /** Null: the user has no personal account (none exists before migration 0042 ran). */
  personalOrganizationId: string | null;
  /** micro-SAR of bought credit that can be moved now, in whole halalas. */
  transferableMicroSar: number;
  destinations: TransferDestination[];
}

const availability = (
  joinedAt: Date,
  lastReceivedAt: Date | null,
  waits: TransferWaits,
  now: Date,
): Pick<TransferDestination, "availableAt" | "waitingFor"> => {
  const tenureEnds =
    joinedAt.getTime() + waits.transferMemberTenureHours * HOUR_MS;
  const cooldownEnds = lastReceivedAt
    ? lastReceivedAt.getTime() + waits.transferCooldownHours * HOUR_MS
    : 0;
  const ends = Math.max(tenureEnds, cooldownEnds);
  if (ends <= now.getTime()) {
    return { availableAt: null, waitingFor: null };
  }
  return {
    availableAt: new Date(ends).toISOString(),
    waitingFor: tenureEnds >= cooldownEnds ? "memberTenure" : "cooldown",
  };
};

/** The user's organizations (never a personal account) with when each joined and last received. */
const destinationsOf = async (
  tx: LedgerTransaction | typeof db,
  userId: string,
  onlyOrganizationId?: string,
) =>
  tx
    .select({
      id: organization.id,
      name: organization.name,
      slug: organization.slug,
      joinedAt: member.createdAt,
      lastReceivedAt: sql<string | null>`(
        select to_char(max(t.created_at), 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
          from ${creditTransfer} t where t.to_organization_id = ${organization.id}
      )`,
    })
    .from(member)
    .innerJoin(organization, eq(organization.id, member.organizationId))
    .where(
      and(
        eq(member.userId, userId),
        onlyOrganizationId
          ? eq(member.organizationId, onlyOrganizationId)
          : undefined,
        sql`not exists (
          select 1 from ${personalAccount}
           where ${personalAccount.organizationId} = ${organization.id}
        )`,
      ),
    )
    .orderBy(asc(organization.name));

/** What the transfer form shows: how much can move, and where to. */
export const getTransferOptions = async (
  userId: string,
  now = new Date(),
): Promise<TransferOptions> => {
  const [personal] = await db
    .select({ organizationId: personalAccount.organizationId })
    .from(personalAccount)
    .where(eq(personalAccount.userId, userId));
  if (!personal) {
    return {
      personalOrganizationId: null,
      transferableMicroSar: 0,
      destinations: [],
    };
  }
  const [lots, [account], destinations, waits] = await Promise.all([
    transferableLots(db, personal.organizationId),
    db
      .select({ balance: creditAccount.balance, held: creditAccount.held })
      .from(creditAccount)
      .where(eq(creditAccount.organizationId, personal.organizationId)),
    destinationsOf(db, userId),
    getWaits(db),
  ]);
  return {
    personalOrganizationId: personal.organizationId,
    transferableMicroSar: transferableOf(lots, account),
    destinations: destinations.map(
      ({ joinedAt, lastReceivedAt, ...destination }) => ({
        ...destination,
        ...availability(
          joinedAt,
          lastReceivedAt ? new Date(lastReceivedAt) : null,
          waits,
          now,
        ),
      }),
    ),
  };
};

export interface TransferCreditsInput {
  /** The user moving credit out of their own personal account. */
  userId: string;
  toOrganizationId: string;
  /** micro-SAR, whole halalas. */
  amountMicroSar: number;
  /** The customer's key for this one move: the same key again moves nothing. */
  idempotencyKey: string;
}

export interface TransferResult {
  /** False when the key had already been applied. */
  isApplied: boolean;
  transferId: string;
  amountMicroSar: number;
  toOrganizationId: string;
}

export const transferCredits = async (
  input: TransferCreditsInput,
  now = new Date(),
): Promise<TransferResult> =>
  db.transaction(async (tx) => {
    const [personal] = await tx
      .select({ organizationId: personalAccount.organizationId })
      .from(personalAccount)
      .where(eq(personalAccount.userId, input.userId));
    if (!personal) {
      throw transferRefusal("noPersonalAccount");
    }
    const fromOrganizationId = personal.organizationId;

    // a retry of the same move: answer with what it did, whatever has changed since
    const [earlier] = await tx
      .select()
      .from(creditTransfer)
      .where(eq(creditTransfer.idempotencyKey, input.idempotencyKey));
    if (earlier) {
      return replayOf(earlier, input, fromOrganizationId);
    }

    const [isPersonalTarget] = await tx
      .select({ userId: personalAccount.userId })
      .from(personalAccount)
      .where(eq(personalAccount.organizationId, input.toOrganizationId));
    if (isPersonalTarget) {
      throw transferRefusal("notAnOrganization");
    }

    // both accounts, in id order, so two transfers cannot wait on each other;
    // the gateway's settlement, the refunds and the expiry job take one each
    await tx
      .insert(creditAccount)
      .values({ organizationId: input.toOrganizationId })
      .onConflictDoNothing({ target: creditAccount.organizationId });
    const accounts = await tx
      .select({
        organizationId: creditAccount.organizationId,
        balance: creditAccount.balance,
        held: creditAccount.held,
      })
      .from(creditAccount)
      .where(
        inArray(creditAccount.organizationId, [
          fromOrganizationId,
          input.toOrganizationId,
        ]),
      )
      .orderBy(asc(creditAccount.organizationId))
      .for("update");

    // read under the lock: the membership, and the organization's last transfer
    const [destination] = await destinationsOf(
      tx,
      input.userId,
      input.toOrganizationId,
    );
    if (!destination) {
      throw transferRefusal("notMember");
    }
    const { waitingFor } = availability(
      destination.joinedAt,
      destination.lastReceivedAt ? new Date(destination.lastReceivedAt) : null,
      await getWaits(tx),
      now,
    );
    if (waitingFor) {
      throw transferRefusal(
        waitingFor === "memberTenure"
          ? "memberTooNew"
          : "organizationCoolingDown",
      );
    }

    const lots = await transferableLots(tx, fromOrganizationId);
    const transferable = transferableOf(
      lots,
      accounts.find((account) => account.organizationId === fromOrganizationId),
    );
    if (input.amountMicroSar > transferable) {
      throw transferRefusal("insufficientCredit");
    }

    const [transfer] = await tx
      .insert(creditTransfer)
      .values({
        fromOrganizationId,
        toOrganizationId: input.toOrganizationId,
        amountMicroSar: input.amountMicroSar,
        requestedBy: input.userId,
        idempotencyKey: input.idempotencyKey,
      })
      .onConflictDoNothing({ target: creditTransfer.idempotencyKey })
      .returning({ id: creditTransfer.id });
    if (!transfer) {
      // the same key, committed by another request while this one waited
      const [winner] = await tx
        .select()
        .from(creditTransfer)
        .where(eq(creditTransfer.idempotencyKey, input.idempotencyKey));
      if (!winner) {
        throw new Error("credit_transfer conflict without a row");
      }
      return replayOf(winner, input, fromOrganizationId);
    }

    let left = input.amountMicroSar;
    for (const { lot, remainingMicroSar } of lots) {
      if (left === 0) {
        break;
      }
      const taken = Math.min(left, remainingMicroSar);
      left -= taken;
      await applyEntryIn(tx, {
        organizationId: fromOrganizationId,
        amount: -taken,
        kind: "transfer_out",
        idempotencyKey: `transfer:${transfer.id}:out:${lot.id}`,
        reference: transfer.id,
        creditLotId: lot.id,
        metadata: { toOrganizationId: input.toOrganizationId },
      });
      await applyEntryIn(tx, {
        organizationId: input.toOrganizationId,
        amount: taken,
        kind: "transfer_in",
        idempotencyKey: `transfer:${transfer.id}:in:${lot.id}`,
        reference: transfer.id,
        metadata: { fromUserId: input.userId, fromLotId: lot.id },
        expiryFromLotId: lot.id,
      });
    }
    if (left !== 0) {
      // transferable was worked out from these same lots under the lock
      throw new Error("credit transfer did not find the credit it had counted");
    }

    return {
      isApplied: true,
      transferId: transfer.id,
      amountMicroSar: input.amountMicroSar,
      toOrganizationId: input.toOrganizationId,
    };
  });

/** A key seen before must name the same move; a different one means two events share an identity. */
const replayOf = (
  earlier: {
    id: string;
    fromOrganizationId: string;
    toOrganizationId: string;
    amountMicroSar: number;
  },
  input: TransferCreditsInput,
  fromOrganizationId: string,
): TransferResult => {
  if (
    earlier.fromOrganizationId !== fromOrganizationId ||
    earlier.toOrganizationId !== input.toOrganizationId ||
    earlier.amountMicroSar !== input.amountMicroSar
  ) {
    throw transferRefusal("keyReused");
  }
  return {
    isApplied: false,
    transferId: earlier.id,
    amountMicroSar: earlier.amountMicroSar,
    toOrganizationId: earlier.toOrganizationId,
  };
};
