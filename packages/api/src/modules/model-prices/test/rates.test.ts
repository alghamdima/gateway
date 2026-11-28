import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

import { TYPED_RATE_FIELDS } from "@workspace/db/schema";

import {
  floatToDecimal,
  isDecimalRate,
  normalizeDecimal,
  OTHER_RATE_FIELDS,
  ratesEqual,
  ratesFromGateway,
  toPatch,
  usdToSar,
} from "../rates";

import type { Rates } from "../rates";

const byName = (a: string, b: string) => a.localeCompare(b);

const rates = (overrides: Partial<Rates> = {}): Rates => ({
  inputUsd: null,
  outputUsd: null,
  cacheReadUsd: null,
  cacheWriteUsd: null,
  cacheWrite1hUsd: null,
  otherRates: {},
  ...overrides,
});

describe("model price rates (ADR 0018)", () => {
  it("allows exactly the cost fields a gateway pricing override can patch", () => {
    // the gateway's Options struct is the patch shape; every *float64 field is a unit
    const source = readFileSync(
      resolve(
        __dirname,
        "../../../../../../apps/gateway/framework/modelcatalog/datasheet/types.go",
      ),
      "utf8",
    );
    const start = source.indexOf("type Options struct {");
    const body = source.slice(start, source.indexOf("\n}\n", start));
    const gatewayFields = [
      ...body.matchAll(/^\s*\w+\s+\*float64\s+`json:"([a-z0-9_]+)/gm),
    ].flatMap((match) => (match[1] ? [match[1]] : []));
    expect(gatewayFields.length).toBeGreaterThan(100);
    const ours = [...Object.values(TYPED_RATE_FIELDS), ...OTHER_RATE_FIELDS];
    expect(new Set(ours).size).toBe(ours.length);
    expect([...ours].sort(byName)).toEqual([...gatewayFields].sort(byName));
  });

  it("keeps a rate exact: the datasheet's floats become their decimals and back", () => {
    for (const value of [
      1.5e-7, 0.000003, 4.9999999999999996e-6, 2.86102294921875e-9,
      8.33333333333333e-8, 5.0000000000000004e-8, 0.04, 1.5, 0,
    ]) {
      const decimal = floatToDecimal(value);
      expect(isDecimalRate(decimal)).toBe(true);
      // the float the gateway is sent is the float the datasheet had
      expect(Number(decimal)).toBe(value);
    }
    expect(floatToDecimal(1.5e-7)).toBe("0.00000015");
  });

  it("normalizes Postgres' padded numerics so equal prices compare equal", () => {
    expect(normalizeDecimal("0.000003000000000000000000000000")).toBe(
      "0.000003",
    );
    expect(normalizeDecimal("1.000")).toBe("1");
    expect(normalizeDecimal("0.000")).toBe("0");
    expect(normalizeDecimal("007")).toBe("7");
    expect(
      ratesEqual(
        rates({ inputUsd: "0.0000030" }),
        rates({ inputUsd: "0.000003" }),
      ),
    ).toBe(true);
    // a rate set to zero is not a rate left out
    expect(
      ratesEqual(rates({ inputUsd: "0" }), rates({ inputUsd: null })),
    ).toBe(false);
    expect(
      ratesEqual(
        rates({ otherRates: { output_cost_per_image: "0.04" } }),
        rates({ otherRates: { output_cost_per_image: "0.040" } }),
      ),
    ).toBe(true);
  });

  it("refuses what Postgres would round or cannot hold", () => {
    expect(isDecimalRate("0.000003")).toBe(true);
    expect(isDecimalRate("1e-7")).toBe(false);
    expect(isDecimalRate("-0.1")).toBe(false);
    expect(isDecimalRate(`0.${"1".repeat(31)}`)).toBe(false);
    expect(isDecimalRate(`0.${"1".repeat(30)}`)).toBe(true);
  });

  it("takes a gateway row's rates by field name, and lists what it cannot take", () => {
    const { rates: taken, skipped } = ratesFromGateway({
      model: "gpt-4o-mini",
      mode: "chat",
      input_cost_per_token: 1.5e-7,
      output_cost_per_token: 6e-7,
      cache_read_input_token_cost: 7.5e-8,
      output_cost_per_image: 0.04,
      input_cost_per_token_batches: 7.5e-8,
      // not a field the push can send, and a token rate above the ceiling
      some_new_cost_per_thing: 1,
      cache_creation_input_token_cost: 5,
    });
    expect(taken).toEqual(
      rates({
        inputUsd: "0.00000015",
        outputUsd: "0.0000006",
        cacheReadUsd: "0.000000075",
        otherRates: {
          input_cost_per_token_batches: "0.000000075",
          output_cost_per_image: "0.04",
        },
      }),
    );
    expect(skipped).toEqual([
      "cache_creation_input_token_cost",
      "some_new_cost_per_thing",
    ]);
    expect(toPatch(taken)).toEqual({
      input_cost_per_token: 1.5e-7,
      output_cost_per_token: 6e-7,
      cache_read_input_token_cost: 7.5e-8,
      input_cost_per_token_batches: 7.5e-8,
      output_cost_per_image: 0.04,
    });
  });

  it("converts a USD rate to SAR exactly, at the effective rate", () => {
    // 0.15 per million × 3.75
    expect(usdToSar("0.00000015", 3_750_000)).toBe("0.0000005625");
    // × 3.84375 (the peg with a 2.5% spread)
    expect(usdToSar("0.00000015", 3_843_750)).toBe("0.0000005765625");
    // a per-pixel rate with 23 decimals keeps every one of them
    expect(usdToSar("0.00000000286102294921875", 3_750_000)).toBe(
      "0.0000000107288360595703125",
    );
    expect(usdToSar("0", 3_750_000)).toBe("0");
    expect(usdToSar("1", 3_750_000)).toBe("3.75");
    expect(() => usdToSar("1", 0)).toThrow("positive integer");
  });
});
