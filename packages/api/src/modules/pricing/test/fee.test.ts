import { describe, expect, it } from "vitest";

import { computeTopupFee } from "../fee";

const USD = 1_000_000;
const openRouter = { topupFeeBps: 550, topupFeeMinMicroSar: 0.8 * USD };

describe("top-up fee", () => {
  it.each([
    // OpenRouter's published rates: 5.5% with a $0.80 minimum
    ["$5 pays the minimum", 5 * USD, 0.8 * USD],
    ["$10 pays the minimum, 5.5% is only $0.55", 10 * USD, 0.8 * USD],
    ["$14.54: 5.5% is $0.7997, still the minimum", 14.54 * USD, 0.8 * USD],
    ["$14.56: 5.5% is $0.8008, rounds to $0.80", 14.56 * USD, 0.8 * USD],
    ["$15: 5.5% is $0.825, rounds half up to $0.83", 15 * USD, 0.83 * USD],
    ["$100 pays $5.50", 100 * USD, 5.5 * USD],
    ["$1,000 pays $55", 1000 * USD, 55 * USD],
  ])("%s", (_, amount, fee) => {
    expect(computeTopupFee(amount, openRouter)).toBe(fee);
  });

  it("is always a whole number of cents", () => {
    for (let cents = 1; cents <= 20_000; cents += 7) {
      const fee = computeTopupFee(cents * 10_000, openRouter);
      expect(fee % 10_000).toBe(0);
      expect(fee).toBeGreaterThanOrEqual(0.8 * USD);
    }
  });

  it("rounds a minimum that is not whole cents up, never down", () => {
    expect(
      computeTopupFee(USD, { topupFeeBps: 0, topupFeeMinMicroSar: 1 }),
    ).toBe(10_000);
  });

  it("charges nothing when both rates are zero", () => {
    expect(
      computeTopupFee(50 * USD, { topupFeeBps: 0, topupFeeMinMicroSar: 0 }),
    ).toBe(0);
  });

  it("charges only the minimum when the percentage is zero", () => {
    expect(
      computeTopupFee(50 * USD, { topupFeeBps: 0, topupFeeMinMicroSar: USD }),
    ).toBe(USD);
  });

  it("is exact for amounts far beyond any float rounding", () => {
    // 9 billion USD in micro-SAR is close to Number.MAX_SAFE_INTEGER
    expect(computeTopupFee(9_000_000_000 * USD, openRouter)).toBe(
      495_000_000 * USD,
    );
  });

  it.each([0, -1, 1.5, Number.NaN])("refuses a top-up of %s", (amount) => {
    expect(() => computeTopupFee(amount, openRouter)).toThrow(
      /positive whole number/,
    );
  });

  it("refuses negative rates", () => {
    expect(() =>
      computeTopupFee(USD, { topupFeeBps: -1, topupFeeMinMicroSar: 0 }),
    ).toThrow(/non-negative/);
  });
});
