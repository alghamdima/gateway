import { and, eq, sql } from "@workspace/db";
import { API_KEY_LIMIT_RESETS, creditLedgerEntry } from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { CURRENCY } from "@workspace/shared/utils";

import { getBalance, MICRO_SAR } from "../credits/ledger";
import { getKeyUsage } from "../keys/usage";

import type { CallingKey } from "./auth";
import type { ApiKeyLimitReset } from "@workspace/db/schema";

/**
 * The public API speaks amounts as JSON numbers, as OpenRouter's, but in the
 * account's currency, SAR, where OpenRouter's are USD: credits, limits and
 * usage are what the ledger holds, and a USD figure would be a conversion at
 * today's rate that changes with it (docs/platform/adr/0017-sar-ledger.md).
 * Each response says its currency.
 */
export const toSar = (microSar: number) => microSar / MICRO_SAR;

/**
 * GET /api/v1/key: OpenRouter's self-inspection shape. limit_remaining is
 * what the credits plugin would still let the key hold in its window
 * (docs/platform/adr/0013-key-limits-in-the-ledger.md): the limit, minus the
 * window's usage, minus open holds. limit_resets_at is when the window starts
 * again (null for a limit that never resets). The figures are the key page's
 * (keys/usage.ts), with the windows the plugin enforces. Amounts are SAR.
 */
export const describeKey = async (key: CallingKey) => {
  const reset = isReset(key.limitReset) ? key.limitReset : null;
  const usage = await getKeyUsage(key.virtualKeyId, reset);

  return {
    data: {
      label: `${key.prefix}…`,
      name: key.name,
      limit: key.limitMicroSar === null ? null : toSar(key.limitMicroSar),
      limit_remaining:
        key.limitMicroSar === null
          ? null
          : toSar(Math.max(0, key.limitMicroSar - usage.spentAgainstLimit)),
      limit_reset: reset,
      limit_resets_at: usage.resetsAt?.toISOString() ?? null,
      usage: toSar(usage.total),
      usage_daily: toSar(usage.daily),
      usage_weekly: toSar(usage.weekly),
      usage_monthly: toSar(usage.monthly),
      currency: CURRENCY,
      is_management_key: false,
      disabled: false,
      expires_at: key.expiresAt?.toISOString() ?? null,
      created_at: key.createdAt.toISOString(),
    },
  };
};

const isReset = (value: string | null): value is ApiKeyLimitReset =>
  API_KEY_LIMIT_RESETS.some((reset) => reset === value);

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
