import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type postgres from "postgres";

// Needs real Postgres: the answers are sums over the ledger. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

describe.skipIf(!adminUrl)("activity", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let activity: typeof import("../queries");
  let seq = 0;

  const NOW = new Date("2026-11-24T15:00:00Z");

  const createOrganization = async () => {
    const id = `org_act_${++seq}`;
    await sql`insert into organization (id, name, slug, created_at) values (${id}, ${id}, ${id}, now())`;
    return id;
  };

  const usage = async (
    organizationId: string,
    entry: {
      amount: number;
      at: string;
      virtualKeyId?: string;
      model?: string;
      provider?: string;
      prompt?: number;
      completion?: number;
      kind?: string;
    },
  ) => {
    const id = `act_${++seq}`;
    await sql`insert into credit_ledger_entry (id, organization_id, amount, kind, idempotency_key, virtual_key_id, metadata, created_at)
      values (${id}, ${organizationId}, ${entry.kind === "top_up" ? entry.amount : -entry.amount}, ${entry.kind ?? "usage"}, ${id},
              ${entry.virtualKeyId ?? null},
              ${sql.json({ model: entry.model ?? "gpt-4o-mini", provider: entry.provider ?? "openai", prompt_tokens: entry.prompt ?? 0, completion_tokens: entry.completion ?? 0 })},
              ${entry.at})`;
  };

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;
    sql = (await import("postgres")).default(database.url, { max: 4 });
    activity = await import("../queries");
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  it("sums the window's usage by day, model and key, and equals the ledger", async () => {
    const org = await createOrganization();
    const other = await createOrganization();
    await sql`insert into api_key (id, organization_id, created_by, name, virtual_key_id, prefix, value_hash)
      values ('key_prod', ${org}, 'u', 'Production', 'vk_prod', 'sk-bf-prod', 'hash_prod')`;

    // inside the 7-day window
    await usage(org, {
      amount: 300,
      at: "2026-11-24T10:00:00Z",
      virtualKeyId: "vk_prod",
      prompt: 1000,
      completion: 500,
    });
    await usage(org, {
      amount: 200,
      at: "2026-11-24T11:00:00Z",
      virtualKeyId: "vk_prod",
      model: "claude-x",
      provider: "anthropic",
      prompt: 400,
      completion: 100,
    });
    await usage(org, {
      amount: 100,
      at: "2026-11-20T23:59:00Z",
      virtualKeyId: "vk_deleted",
      prompt: 50,
      completion: 50,
    });
    await usage(org, {
      amount: 50,
      at: "2026-11-18T00:00:00Z",
      virtualKeyId: "vk_prod",
      prompt: 10,
      completion: 5,
    });
    // outside it, not usage, or not this organization
    await usage(org, {
      amount: 999,
      at: "2026-11-17T23:59:59Z",
      virtualKeyId: "vk_prod",
    });
    await usage(org, {
      amount: 5000,
      at: "2026-11-24T09:00:00Z",
      kind: "top_up",
    });
    await usage(other, { amount: 777, at: "2026-11-24T09:00:00Z" });

    const result = await activity.getActivity({
      organizationId: org,
      days: 7,
      now: NOW,
    });

    expect(result.from).toBe("2026-11-18T00:00:00.000Z");
    expect(result.totals).toEqual({
      spend: 650,
      requests: 4,
      promptTokens: 1460,
      completionTokens: 655,
    });

    // the same numbers straight from the ledger
    const [ledger] = await sql<{ spend: string; requests: string }[]>`
      select -sum(amount) as spend, count(*) as requests from credit_ledger_entry
      where organization_id = ${org} and kind = 'usage' and created_at >= '2026-11-18'`;
    expect(result.totals.spend).toBe(Number(ledger?.spend));
    expect(result.totals.requests).toBe(Number(ledger?.requests));

    expect(result.byDay).toHaveLength(7);
    expect(result.byDay.map((d) => [d.day, d.spend])).toEqual([
      ["2026-11-18", 50],
      ["2026-11-19", 0],
      ["2026-11-20", 100],
      ["2026-11-21", 0],
      ["2026-11-22", 0],
      ["2026-11-23", 0],
      ["2026-11-24", 500],
    ]);

    expect(result.byModel).toEqual([
      {
        model: "gpt-4o-mini",
        provider: "openai",
        spend: 450,
        requests: 3,
        promptTokens: 1060,
        completionTokens: 555,
      },
      {
        model: "claude-x",
        provider: "anthropic",
        spend: 200,
        requests: 1,
        promptTokens: 400,
        completionTokens: 100,
      },
    ]);

    expect(result.byKey).toEqual([
      {
        keyId: "key_prod",
        name: "Production",
        prefix: "sk-bf-prod",
        spend: 550,
        requests: 3,
        promptTokens: 1410,
        completionTokens: 605,
      },
      // a key that no longer exists keeps its spend, without a name
      {
        keyId: null,
        name: null,
        prefix: null,
        spend: 100,
        requests: 1,
        promptTokens: 50,
        completionTokens: 50,
      },
    ]);
  });

  it("is all zeros, with every day present, for an organization that has not used anything", async () => {
    const org = await createOrganization();
    const result = await activity.getActivity({
      organizationId: org,
      days: 30,
      now: NOW,
    });
    expect(result.totals).toEqual({
      spend: 0,
      requests: 0,
      promptTokens: 0,
      completionTokens: 0,
    });
    expect(result.byDay).toHaveLength(30);
    expect(result.byDay.every((d) => d.spend === 0)).toBe(true);
    expect(result.byModel).toEqual([]);
    expect(result.byKey).toEqual([]);
  });
});
