import { and, eq, gte, isNull, sql } from "@workspace/db";
import { creditLedgerEntry, creditReservation } from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { CURRENCY } from "@workspace/shared/utils";

import { getBalance, MICRO_SAR } from "../credits/ledger";

import type { CallingKey } from "./auth";

/**
 * The public API speaks amounts as JSON numbers, as OpenRouter's, but in the
 * account's currency, SAR, where OpenRouter's are USD: credits, limits and
 * usage are what the ledger holds, and a USD figure would be a conversion at
 * today's rate that changes with it (docs/platform/adr/0017-sar-ledger.md).
 * Each response says its currency.
 */
export const toSar = (microSar: number) => microSar / MICRO_SAR;

const WINDOW_START = {
  daily: sql`date_trunc('day', now() AT TIME ZONE 'UTC')`,
  weekly: sql`date_trunc('week', now() AT TIME ZONE 'UTC')`,
  monthly: sql`date_trunc('month', now() AT TIME ZONE 'UTC')`,
} as const;

/** What a key spent since `since` (all time without it), in micro-SAR, from its usage entries. */
const keyUsage = async (
  virtualKeyId: string,
  since?: keyof typeof WINDOW_START,
) => {
  const [row] = await db
    .select({
      total: sql<string>`coalesce(-sum(${creditLedgerEntry.amount}), 0)`,
    })
    .from(creditLedgerEntry)
    .where(
      and(
        eq(creditLedgerEntry.virtualKeyId, virtualKeyId),
        eq(creditLedgerEntry.kind, "usage"),
        since
          ? gte(creditLedgerEntry.createdAt, WINDOW_START[since])
          : undefined,
      ),
    );
  return Number(row?.total ?? 0);
};

const keyHeld = async (virtualKeyId: string) => {
  const [row] = await db
    .select({
      total: sql<string>`coalesce(sum(${creditReservation.amount}), 0)`,
    })
    .from(creditReservation)
    .where(
      and(
        eq(creditReservation.virtualKeyId, virtualKeyId),
        isNull(creditReservation.settledAt),
      ),
    );
  return Number(row?.total ?? 0);
};

const isWindow = (value: string | null): value is keyof typeof WINDOW_START =>
  value !== null && value in WINDOW_START;

/**
 * When a key's limit window resets: the next UTC midnight, the next ISO week
 * (Monday) or the next calendar month, the windows the credits plugin counts
 * in (apps/gateway/plugins/credits/refusals.go, windowEnd).
 */
export const windowEnd = (
  reset: keyof typeof WINDOW_START,
  now = new Date(),
) => {
  const day = Date.UTC(
    now.getUTCFullYear(),
    now.getUTCMonth(),
    now.getUTCDate(),
  );
  const DAY = 86_400_000;
  if (reset === "daily") {
    return new Date(day + DAY);
  }
  if (reset === "weekly") {
    const sinceMonday = (now.getUTCDay() + 6) % 7;
    return new Date(day + (7 - sinceMonday) * DAY);
  }
  return new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() + 1, 1));
};

/**
 * GET /api/v1/key: OpenRouter's self-inspection shape. limit_remaining is
 * what the credits plugin would still let the key hold in its window
 * (docs/platform/adr/0013-key-limits-in-the-ledger.md): the limit, minus the
 * window's usage, minus open holds. limit_resets_at is when the window starts
 * again (null for a limit that never resets). Amounts are SAR.
 */
export const describeKey = async (key: CallingKey) => {
  const reset = isWindow(key.limitReset) ? key.limitReset : null;
  const [usage, daily, weekly, monthly, held] = await Promise.all([
    keyUsage(key.virtualKeyId),
    keyUsage(key.virtualKeyId, "daily"),
    keyUsage(key.virtualKeyId, "weekly"),
    keyUsage(key.virtualKeyId, "monthly"),
    keyHeld(key.virtualKeyId),
  ]);
  const windowUsage = { daily, weekly, monthly }[reset ?? "daily"];
  const spentAgainstLimit = (reset ? windowUsage : usage) + held;

  return {
    data: {
      label: `${key.prefix}…`,
      name: key.name,
      limit: key.limitMicroSar === null ? null : toSar(key.limitMicroSar),
      limit_remaining:
        key.limitMicroSar === null
          ? null
          : toSar(Math.max(0, key.limitMicroSar - spentAgainstLimit)),
      limit_reset: reset,
      limit_resets_at: reset ? windowEnd(reset).toISOString() : null,
      usage: toSar(usage),
      usage_daily: toSar(daily),
      usage_weekly: toSar(weekly),
      usage_monthly: toSar(monthly),
      currency: CURRENCY,
      is_management_key: false,
      disabled: false,
      expires_at: key.expiresAt?.toISOString() ?? null,
      created_at: key.createdAt.toISOString(),
    },
  };
};

/**
 * GET /api/v1/credits: OpenRouter's two running totals, for the key's
 * organization. total_credits is defined so that total_credits - total_usage
 * is the balance: it nets refunds and staff adjustments into what was bought.
 */
export const describeCredits = async (organizationId: string) => {
  const [{ balance }, [row]] = await Promise.all([
    getBalance(organizationId),
    db
      .select({
        usage: sql<string>`coalesce(-sum(${creditLedgerEntry.amount}), 0)`,
      })
      .from(creditLedgerEntry)
      .where(
        and(
          eq(creditLedgerEntry.organizationId, organizationId),
          eq(creditLedgerEntry.kind, "usage"),
        ),
      ),
  ]);
  const usage = Number(row?.usage ?? 0);
  return {
    data: {
      total_credits: toSar(balance + usage),
      total_usage: toSar(usage),
      currency: CURRENCY,
    },
  };
};
