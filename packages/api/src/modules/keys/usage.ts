import { and, eq, gte, isNull, sql } from "@workspace/db";
import {
  apiKey,
  creditLedgerEntry,
  creditReservation,
} from "@workspace/db/schema";
import { db } from "@workspace/db/server";

import type { ApiKeyLimitReset } from "@workspace/db/schema";

/**
 * What a key has spent, read from the ledger (KEYS-06, KEYS-11, GET
 * /api/v1/key). A key's spend is the sum of its usage entries, each charged
 * by the credits plugin (ADR 0013); open holds count against the limit, as
 * the plugin counts them. The windows (UTC day, ISO week from Monday,
 * calendar month) are the database's key_limit_window_start and
 * key_limit_window_end (migration 0040), the functions the credits plugin
 * enforces the limit with, so the figures here and the plugin's refusals
 * cannot disagree on where a window starts.
 *
 * Last used is the time of the key's latest usage entry, read through the
 * partial index on (virtual_key_id, created_at): no column on api_key is
 * written on every request, so the gateway's hot path is unchanged.
 */

const DAY_MS = 86_400_000;
/** How many UTC days the spend chart covers, today included. */
export const SPEND_CHART_DAYS = 30;

const utcNow = sql`now() AT TIME ZONE 'UTC'`;
const windowStart = (reset: ApiKeyLimitReset) =>
  sql`key_limit_window_start(${reset}, ${utcNow})`;

const spent = sql<string>`coalesce(-sum(${creditLedgerEntry.amount}), 0)`;
const spentSince = (reset: ApiKeyLimitReset) =>
  sql<string>`coalesce(-sum(${creditLedgerEntry.amount}) filter (where ${creditLedgerEntry.createdAt} >= ${windowStart(reset)}), 0)`;

export interface KeyUsage {
  /** micro-SAR, all time. */
  total: number;
  /** micro-SAR, this UTC day, ISO week and calendar month. */
  daily: number;
  weekly: number;
  monthly: number;
  /** micro-SAR the key's open holds may still spend. */
  held: number;
  /**
   * What counts against the limit now, as the credits plugin counts it: the
   * limit window's usage (all time without a reset) plus open holds.
   */
  spentAgainstLimit: number;
  /** When the limit window starts again; null for no reset. */
  resetsAt: Date | null;
  /** The latest charged request; null: never used. */
  lastUsedAt: Date | null;
}

const heldBy = async (virtualKeyId: string) => {
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

/** One key's usage, for its page and for GET /api/v1/key. */
export const getKeyUsage = async (
  virtualKeyId: string,
  reset: ApiKeyLimitReset | null,
): Promise<KeyUsage> => {
  const [[row], held] = await Promise.all([
    db
      .select({
        total: spent,
        daily: spentSince("daily"),
        weekly: spentSince("weekly"),
        monthly: spentSince("monthly"),
        lastUsedAt:
          sql<Date | null>`max(${creditLedgerEntry.createdAt})`.mapWith(
            creditLedgerEntry.createdAt,
          ),
        resetsAt:
          sql<Date | null>`key_limit_window_end(${reset}, ${utcNow})`.mapWith(
            creditLedgerEntry.createdAt,
          ),
      })
      .from(creditLedgerEntry)
      .where(
        and(
          eq(creditLedgerEntry.virtualKeyId, virtualKeyId),
          eq(creditLedgerEntry.kind, "usage"),
        ),
      ),
    heldBy(virtualKeyId),
  ]);
  const usage = {
    total: Number(row?.total ?? 0),
    daily: Number(row?.daily ?? 0),
    weekly: Number(row?.weekly ?? 0),
    monthly: Number(row?.monthly ?? 0),
  };
  return {
    ...usage,
    held,
    spentAgainstLimit: (reset ? usage[reset] : usage.total) + held,
    resetsAt: row?.resetsAt ?? null,
    lastUsedAt: row?.lastUsedAt ?? null,
  };
};

export interface KeyListUsage {
  /** micro-SAR, all time. */
  total: number;
  /** The limit window's usage plus open holds, as the plugin counts it. */
  spentAgainstLimit: number;
  lastUsedAt: Date | null;
}

/** Every key of an organization with its usage, in one query, keyed by key id. */
export const getOrganizationKeyUsage = async (organizationId: string) => {
  const held = db
    .select({
      virtualKeyId: creditReservation.virtualKeyId,
      amount: sql<string>`sum(${creditReservation.amount})`.as("held_amount"),
    })
    .from(creditReservation)
    .where(
      and(
        eq(creditReservation.organizationId, organizationId),
        isNull(creditReservation.settledAt),
      ),
    )
    .groupBy(creditReservation.virtualKeyId)
    .as("held");
  const rows = await db
    .select({
      id: apiKey.id,
      total: spent,
      window: sql<string>`coalesce(-sum(${creditLedgerEntry.amount}) filter (where ${creditLedgerEntry.createdAt} >= key_limit_window_start(${apiKey.limitReset}, ${utcNow})), 0)`,
      held: sql<string>`coalesce(max(${held.amount}), 0)`,
      lastUsedAt: sql<Date | null>`max(${creditLedgerEntry.createdAt})`.mapWith(
        creditLedgerEntry.createdAt,
      ),
    })
    .from(apiKey)
    .leftJoin(
      creditLedgerEntry,
      and(
        eq(creditLedgerEntry.virtualKeyId, apiKey.virtualKeyId),
        eq(creditLedgerEntry.kind, "usage"),
      ),
    )
    .leftJoin(held, eq(held.virtualKeyId, apiKey.virtualKeyId))
    .where(eq(apiKey.organizationId, organizationId))
    .groupBy(apiKey.id);
  return new Map<string, KeyListUsage>(
    rows.map((row) => [
      row.id,
      {
        total: Number(row.total),
        spentAgainstLimit: Number(row.window) + Number(row.held),
        lastUsedAt: row.lastUsedAt,
      },
    ]),
  );
};

/** The key's spend per UTC day over the last 30 days, today included, zero where nothing ran. */
export const getKeySpendByDay = async (
  virtualKeyId: string,
  now = new Date(),
) => {
  const today = Date.UTC(
    now.getUTCFullYear(),
    now.getUTCMonth(),
    now.getUTCDate(),
  );
  const from = new Date(today - (SPEND_CHART_DAYS - 1) * DAY_MS);
  const day = sql<string>`to_char(date_trunc('day', ${creditLedgerEntry.createdAt}), 'YYYY-MM-DD')`;
  const rows = await db
    .select({ day, spend: spent })
    .from(creditLedgerEntry)
    .where(
      and(
        eq(creditLedgerEntry.virtualKeyId, virtualKeyId),
        eq(creditLedgerEntry.kind, "usage"),
        gte(creditLedgerEntry.createdAt, from),
      ),
    )
    .groupBy(day);
  const byDate = new Map(rows.map((row) => [row.day, Number(row.spend)]));
  return Array.from({ length: SPEND_CHART_DAYS }, (_, i) => {
    const date = new Date(from.getTime() + i * DAY_MS)
      .toISOString()
      .slice(0, 10);
    return { day: date, spend: byDate.get(date) ?? 0 };
  });
};
