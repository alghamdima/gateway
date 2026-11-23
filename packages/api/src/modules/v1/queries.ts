import { and, eq, gte, isNull, sql } from "@workspace/db";
import { creditLedgerEntry, creditReservation } from "@workspace/db/schema";
import { db } from "@workspace/db/server";

import { getBalance, MICRO_USD } from "../credits/ledger";

import type { CallingKey } from "./auth";

/** The public API speaks dollars as JSON numbers, as OpenRouter's; the ledger keeps micro-USD. */
export const toUsd = (microUsd: number) => microUsd / MICRO_USD;

const WINDOW_START = {
  daily: sql`date_trunc('day', now() AT TIME ZONE 'UTC')`,
  weekly: sql`date_trunc('week', now() AT TIME ZONE 'UTC')`,
  monthly: sql`date_trunc('month', now() AT TIME ZONE 'UTC')`,
} as const;

/** What a key spent since `since` (all time without it), in micro-USD, from its usage entries. */
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
 * GET /api/v1/key: OpenRouter's self-inspection shape. limit_remaining is
 * what the credits plugin would still let the key hold in its window
 * (docs/platform/adr/0013-key-limits-in-the-ledger.md): the limit, minus the
 * window's usage, minus open holds.
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
      limit: key.limitMicroUsd === null ? null : toUsd(key.limitMicroUsd),
      limit_remaining:
        key.limitMicroUsd === null
          ? null
          : toUsd(Math.max(0, key.limitMicroUsd - spentAgainstLimit)),
      limit_reset: reset,
      usage: toUsd(usage),
      usage_daily: toUsd(daily),
      usage_weekly: toUsd(weekly),
      usage_monthly: toUsd(monthly),
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
    data: { total_credits: toUsd(balance + usage), total_usage: toUsd(usage) },
  };
};
