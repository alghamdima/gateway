import { describe, expect, it } from "vitest";

import { marketPrice, marketPriceSar } from "../price";

describe("marketPrice", () => {
  it.each([
    // [cost per token, markup bps, shown price]
    [0.00000015, 0, "0.00000015"],
    [0.00000015, 500, "0.0000001575"],
    [0.0000006, 550, "0.000000633"],
    [1.014e-9, 100, "0.00000000102414"],
    [0.018, 0, "0.018"],
    [0.018, 1, "0.0180018"],
    [0, 500, "0"],
    [2.5, 1000, "2.75"],
    [10, 0, "10"],
  ])("%s at %s bps is %s", (cost, bps, expected) => {
    expect(marketPrice(cost, bps)).toBe(expected);
  });

  it("is exact where floating point is not", () => {
    // 0.1 × 1.1 in floating point is 0.11000000000000001
    expect(marketPrice(0.1, 1000)).toBe("0.11");
  });

  it("refuses a negative or non-finite cost and a bad markup", () => {
    expect(() => marketPrice(-1, 0)).toThrow("non-negative number");
    expect(() => marketPrice(Number.NaN, 0)).toThrow("non-negative number");
    expect(() => marketPrice(1, -1)).toThrow("basis points");
    expect(() => marketPrice(1, 1.5)).toThrow("basis points");
  });
});

describe("marketPriceSar", () => {
  it.each([
    // 2 USD per million at the peg is SAR 7.50 per million
    [0.000002, 0, 3_750_000, "0.0000075"],
    [0.00000015, 500, 3_750_000, "0.000000590625"],
    // the effective rate with the bank's 2.5%
    [0.00000015, 500, 3_843_750, "0.000000605390625"],
    [0.1, 1000, 3_750_000, "0.4125"],
    [0, 0, 3_750_000, "0"],
  ])("%s at %s bps and rate %s is %s", (cost, bps, rate, expected) => {
    expect(marketPriceSar(cost, bps, rate)).toBe(expected);
  });

  it("refuses a missing rate", () => {
    expect(() => marketPriceSar(1, 0, 0)).toThrow("rate");
    expect(() => marketPriceSar(1, 0, 3.75)).toThrow("rate");
  });
});
