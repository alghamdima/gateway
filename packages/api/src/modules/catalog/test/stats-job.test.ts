import { afterAll, beforeAll, beforeEach, describe, expect, it } from "vitest";

import type { TestDatabase } from "@workspace/db/testing";
import type {
  GatewayClient,
  GatewayEndpointHour,
  GatewayEndpointStats,
} from "@workspace/gateway-client/server";
import type postgres from "postgres";

// The statistics job against real Postgres, with a gateway that answers from
// a list of hours. Skips without DATABASE_URL.
const adminUrl = process.env.DATABASE_URL;

const HOUR = 3_600_000;
const BUCKETS = 64;
const bounds = (first: number) =>
  Array.from({ length: BUCKETS - 1 }, (_, i) => first * 1.2 ** i);
const histogram = (slot: number, calls: number) =>
  Array.from({ length: BUCKETS }, (_, i) => (i === slot ? calls : 0));

const endpointHour = (
  hour: Date,
  overrides: Partial<GatewayEndpointHour> = {},
): GatewayEndpointHour => ({
  hour: hour.toISOString(),
  provider: "openai",
  model: "gpt-4o",
  key_id: "key_a",
  service_tier: "",
  attempts: 3,
  succeeded: 2,
  streamed: 1,
  prompt_tokens: 1500,
  completion_tokens: 500,
  reasoning_tokens: 150,
  cached_read_tokens: 600,
  input_cost_usd: 0.003,
  output_cost_usd: 0.005,
  cost_usd: 0.008,
  latency_ms: histogram(20, 2),
  time_to_first_token_ms: histogram(14, 1),
  throughput_tokens_per_second: histogram(30, 2),
  ...overrides,
});

describe.skipIf(!adminUrl)("endpoint statistics job (CAT-11, CAT-12)", () => {
  let database: TestDatabase;
  let sql: postgres.Sql;
  let job: typeof import("../stats-job");
  /** What the gateway's log holds, and every window it was asked for. */
  let logged: GatewayEndpointHour[];
  let asked: [string, string][];
  let latencyBounds: number[];

  const gateway = {
    getEndpointStats: async (
      from: Date,
      to: Date,
    ): Promise<GatewayEndpointStats> => {
      asked.push([from.toISOString(), to.toISOString()]);
      return {
        bounds: {
          latency_ms: latencyBounds,
          throughput_tokens_per_second: bounds(0.5),
        },
        hours: logged.filter(
          (hour) => new Date(hour.hour) >= from && new Date(hour.hour) < to,
        ),
      };
    },
  } as unknown as GatewayClient;

  const stored = () =>
    sql`select to_char(hour at time zone 'UTC', 'YYYY-MM-DD"T"HH24') as hour, provider_key_id, attempts::int as attempts
          from model_endpoint_stat_hourly order by hour, provider_key_id`;
  const filledThrough = async () => {
    const [row] = await sql<{ filled: Date | null }[]>`
      select filled_through as filled from endpoint_stat_state`;
    return row?.filled?.toISOString() ?? null;
  };

  const now = new Date("2026-11-27T10:20:00Z");
  const ten = new Date("2026-11-27T10:00:00Z");
  const hoursAgo = (hours: number) => new Date(ten.getTime() - hours * HOUR);

  beforeAll(async () => {
    const { createTestDatabase } = await import("@workspace/db/testing");
    database = await createTestDatabase(adminUrl ?? "");
    process.env.DATABASE_URL = database.url;

    sql = (await import("postgres")).default(database.url, { max: 2 });
    job = await import("../stats-job");
  });

  beforeEach(async () => {
    logged = [];
    asked = [];
    latencyBounds = bounds(20);
    await sql`delete from model_endpoint_stat_hourly`;
    await sql`update endpoint_stat_state set filled_through = null, latency_bounds_ms = null, throughput_bounds = null`;
  });

  afterAll(async () => {
    const { db } = await import("@workspace/db/server");
    await db.$client.end();
    await sql.end();
    await database.drop();
  });

  it("reads back 31 days the first time, in day-long windows, and stores each endpoint hour whole", async () => {
    logged = [
      endpointHour(hoursAgo(30 * 24)),
      endpointHour(hoursAgo(1)),
      endpointHour(hoursAgo(1), {
        key_id: "key_b",
        attempts: 1,
        succeeded: 1,
      }),
      endpointHour(ten),
    ];

    const result = await job.runEndpointStatsJob(gateway, now);

    expect(result).toEqual({
      from: "2026-10-27T11:00:00.000Z",
      to: "2026-11-27T11:00:00.000Z",
      rows: 4,
    });
    expect(asked).toHaveLength(31);
    expect(asked.at(0)).toEqual([
      "2026-10-27T11:00:00.000Z",
      "2026-10-28T11:00:00.000Z",
    ]);
    expect(await stored()).toEqual([
      { hour: "2026-10-28T10", provider_key_id: "key_a", attempts: 3 },
      { hour: "2026-11-27T09", provider_key_id: "key_a", attempts: 3 },
      { hour: "2026-11-27T09", provider_key_id: "key_b", attempts: 1 },
      { hour: "2026-11-27T10", provider_key_id: "key_a", attempts: 3 },
    ]);
    const [row] = await sql`
      select prompt_tokens::int as prompt, reasoning_tokens::int as reasoning, cost_usd::float8 as cost,
             latency_ms[21]::int as latency_slot, cardinality(throughput) as buckets
        from model_endpoint_stat_hourly where hour = ${ten}`;
    expect(row).toEqual({
      prompt: 1500,
      reasoning: 150,
      cost: 0.008,
      latency_slot: 2,
      buckets: BUCKETS,
    });
    // the hour in progress is stored, and does not count as filled
    expect(await filledThrough()).toBe("2026-11-27T10:00:00.000Z");
  });

  it("changes nothing when it runs again", async () => {
    logged = [endpointHour(hoursAgo(1)), endpointHour(ten)];
    await job.runEndpointStatsJob(gateway, now);
    const first = await stored();

    const again = await job.runEndpointStatsJob(gateway, now);

    expect(await stored()).toEqual(first);
    expect(again.rows).toBe(2);
  });

  it("reads the last hours again, so rows the gateway wrote late are picked up", async () => {
    logged = [endpointHour(hoursAgo(1), { attempts: 3 })];
    await job.runEndpointStatsJob(gateway, now);
    asked = [];
    // a request of 09:xx that ended after the first run
    logged = [endpointHour(hoursAgo(1), { attempts: 4, succeeded: 3 })];

    const result = await job.runEndpointStatsJob(
      gateway,
      new Date(now.getTime() + 5 * 60_000),
    );

    expect(result.from).toBe("2026-11-27T07:00:00.000Z");
    expect(asked).toEqual([
      ["2026-11-27T07:00:00.000Z", "2026-11-27T11:00:00.000Z"],
    ]);
    expect(await stored()).toEqual([
      { hour: "2026-11-27T09", provider_key_id: "key_a", attempts: 4 },
    ]);
  });

  it("after a long stop, continues from where it stopped and leaves older hours as they are", async () => {
    // hours stored before the stop, which the gateway's log no longer holds
    logged = [endpointHour(hoursAgo(50)), endpointHour(hoursAgo(49))];
    await job.runEndpointStatsJob(gateway, new Date(now.getTime() - 48 * HOUR));
    asked = [];
    logged = [endpointHour(hoursAgo(2))];

    const result = await job.runEndpointStatsJob(gateway, now);

    // filled through 48 hours ago: the window starts there, not three hours before it
    expect(result.from).toBe("2026-11-25T10:00:00.000Z");
    expect(await stored()).toEqual([
      { hour: "2026-11-25T08", provider_key_id: "key_a", attempts: 3 },
      { hour: "2026-11-25T09", provider_key_id: "key_a", attempts: 3 },
      { hour: "2026-11-27T08", provider_key_id: "key_a", attempts: 3 },
    ]);
    expect(await filledThrough()).toBe("2026-11-27T10:00:00.000Z");
  });

  it("stops rather than mix histograms on two scales", async () => {
    logged = [endpointHour(hoursAgo(1))];
    await job.runEndpointStatsJob(gateway, now);
    const before = await stored();
    latencyBounds = bounds(10);
    logged = [endpointHour(hoursAgo(1), { attempts: 9, succeeded: 9 })];

    await expect(job.runEndpointStatsJob(gateway, now)).rejects.toThrow(
      /histogram bounds differ/,
    );

    expect(await stored()).toEqual(before);
  });
});
