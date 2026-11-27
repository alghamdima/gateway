import { and, desc, eq, gte, sql } from "@workspace/db";
import { apiKey, creditLedgerEntry } from "@workspace/db/schema";
import { db } from "@workspace/db/server";

/**
 * The activity page (step 9.1): spend, requests and tokens over a window, by
 * day, by model and by key. Read from the ledger's usage entries, which are
 * kept for good and carry the key and token counts (ADR 0013, 0014), not from
 * the gateway's logs, which expire. Amounts are micro-SAR.
 */

export interface ActivityFigures {
  spend: number;
  requests: number;
  promptTokens: number;
  completionTokens: number;
}

export interface Activity {
  from: string;
  to: string;
  totals: ActivityFigures;
  byDay: (ActivityFigures & { day: string })[];
  byModel: (ActivityFigures & { model: string; provider: string })[];
  byKey: (ActivityFigures & {
    keyId: string | null;
    name: string | null;
    prefix: string | null;
  })[];
}

const DAY_MS = 86_400_000;

const figures = {
  spend: sql<string>`coalesce(-sum(${creditLedgerEntry.amount}), 0)`,
  requests: sql<string>`count(*)`,
  promptTokens: sql<string>`coalesce(sum((${creditLedgerEntry.metadata}->>'prompt_tokens')::bigint), 0)`,
  completionTokens: sql<string>`coalesce(sum((${creditLedgerEntry.metadata}->>'completion_tokens')::bigint), 0)`,
};

const toFigures = (row: {
  spend: string;
  requests: string;
  promptTokens: string;
  completionTokens: string;
}): ActivityFigures => ({
  spend: Number(row.spend),
  requests: Number(row.requests),
  promptTokens: Number(row.promptTokens),
  completionTokens: Number(row.completionTokens),
});

const EMPTY: ActivityFigures = {
  spend: 0,
  requests: 0,
  promptTokens: 0,
  completionTokens: 0,
};

/** The last `days` UTC days, today included. */
export const getActivity = async ({
  organizationId,
  days,
  now = new Date(),
}: {
  organizationId: string;
  days: number;
  now?: Date;
}): Promise<Activity> => {
  const today = Date.UTC(
    now.getUTCFullYear(),
    now.getUTCMonth(),
    now.getUTCDate(),
  );
  const from = new Date(today - (days - 1) * DAY_MS);
  const usage = and(
    eq(creditLedgerEntry.organizationId, organizationId),
    eq(creditLedgerEntry.kind, "usage"),
    gte(creditLedgerEntry.createdAt, from),
  );
  const day = sql<string>`to_char(date_trunc('day', ${creditLedgerEntry.createdAt}), 'YYYY-MM-DD')`;
  const model = sql<string>`coalesce(${creditLedgerEntry.metadata}->>'model', '')`;
  const provider = sql<string>`coalesce(${creditLedgerEntry.metadata}->>'provider', '')`;

  const [totals, daily, models, keys] = await Promise.all([
    db.select(figures).from(creditLedgerEntry).where(usage),
    db
      .select({ day, ...figures })
      .from(creditLedgerEntry)
      .where(usage)
      .groupBy(day),
    db
      .select({ model, provider, ...figures })
      .from(creditLedgerEntry)
      .where(usage)
      .groupBy(model, provider)
      .orderBy(desc(figures.spend)),
    db
      .select({
        keyId: apiKey.id,
        name: apiKey.name,
        prefix: apiKey.prefix,
        ...figures,
      })
      .from(creditLedgerEntry)
      // a deleted key keeps its spend, under no name
      .leftJoin(apiKey, eq(apiKey.virtualKeyId, creditLedgerEntry.virtualKeyId))
      .where(usage)
      .groupBy(apiKey.id, apiKey.name, apiKey.prefix)
      .orderBy(desc(figures.spend)),
  ]);

  // every day of the window, zero where nothing ran, so a chart has no gaps
  const byDate = new Map(daily.map((row) => [row.day, toFigures(row)]));
  const byDay = Array.from({ length: days }, (_, i) => {
    const date = new Date(from.getTime() + i * DAY_MS)
      .toISOString()
      .slice(0, 10);
    return { day: date, ...(byDate.get(date) ?? EMPTY) };
  });

  return {
    from: from.toISOString(),
    to: now.toISOString(),
    totals: totals[0] ? toFigures(totals[0]) : EMPTY,
    byDay,
    byModel: models.map((row) => ({
      model: row.model,
      provider: row.provider,
      ...toFigures(row),
    })),
    byKey: keys.map((row) => ({
      keyId: row.keyId,
      name: row.name,
      prefix: row.prefix,
      ...toFigures(row),
    })),
  };
};
