import { describe, expect, it } from "vitest";

import { marketPrice, marketPriceSar } from "../price";

describe("marketPrice", () => {
  it.each([
    // [cost per token, shown price]: the provider's price, no markup
    [0.00000015, "0.00000015"],
    [0.0000006, "0.0000006"],
    [1.014e-9, "0.000000001014"],
    [0.018, "0.018"],
    [0, "0"],
    [2.5, "2.5"],
    [10, "10"],
    [1e21, "1000000000000000000000"],
  ])("%s is shown as %s", (cost, expected) => {
    expect(marketPrice(cost)).toBe(expected);
  });

  it("refuses a negative or non-finite cost", () => {
    expect(() => marketPrice(-1)).toThrow("non-negative number");
    expect(() => marketPrice(Number.NaN)).toThrow("non-negative number");
    expect(() => marketPrice(Number.POSITIVE_INFINITY)).toThrow(
      "non-negative number",
    );
  });
});

describe("marketPriceSar", () => {
  it.each([
    // the provider's price × the effective rate, exactly, with no markup
    // 2 USD per million at the peg is SAR 7.50 per million
    [0.000002, 3_750_000, "0.0000075"],
    [0.00000015, 3_750_000, "0.0000005625"],
    // the effective rate with the bank's 2.5%; in floating point 5.765624999999999e-7
    [0.00000015, 3_843_750, "0.0000005765625"],
    // in floating point 3.8025000000000005e-9
    [1.014e-9, 3_750_000, "0.0000000038025"],
    [0, 3_750_000, "0"],
  ])("%s at rate %s is %s", (cost, rate, expected) => {
    expect(marketPriceSar(cost, rate)).toBe(expected);
  });

  it("refuses a missing rate", () => {
    expect(() => marketPriceSar(1, 0)).toThrow("rate");
    expect(() => marketPriceSar(1, 3.75)).toThrow("rate");
  });
});
