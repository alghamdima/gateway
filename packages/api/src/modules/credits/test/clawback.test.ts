import { describe, expect, it } from "vitest";

import { bonusClawbackDue } from "../clawback";

// What a refund takes back of a top-up's promo bonus (ADR 0019), without a database.
const SAR = 1_000_000;

const due = (
  refundedCreditMicroSar: number,
  {
    purchase = 100 * SAR,
    bonus = 10 * SAR,
    already = 0,
    remaining = bonus,
  }: {
    purchase?: number;
    bonus?: number;
    already?: number;
    remaining?: number;
  } = {},
) =>
  bonusClawbackDue({
    purchaseGrantedMicroSar: purchase,
    bonusGrantedMicroSar: bonus,
    refundedCreditMicroSar,
    alreadyClawedBackMicroSar: already,
    bonusRemainingMicroSar: remaining,
  });

describe("bonusClawbackDue", () => {
  it("takes back the whole bonus with the whole credit, and nothing with none", () => {
    expect(due(100 * SAR)).toBe(10 * SAR);
    expect(due(0)).toBe(0);
  });

  it("takes back the refunded share of the bonus, rounded down to the halala", () => {
    expect(due(30 * SAR)).toBe(3 * SAR);
    // 10 × 33.333 / 100 = 3.3333: SAR 3.33
    expect(due(33_333_000)).toBe(3_330_000);
    // a share under a halala takes back nothing
    expect(due(99 * 1000)).toBe(0);
  });

  it("counts what earlier refunds took back, so the shares add up to the bonus", () => {
    expect(due(53_330_000, { already: 3 * SAR })).toBe(2_330_000);
    expect(due(100 * SAR, { already: 5_330_000 })).toBe(4_670_000);
    expect(due(100 * SAR, { already: 10 * SAR })).toBe(0);
  });

  it("never takes back more than is left of the bonus: what was spent stays spent", () => {
    expect(due(100 * SAR, { remaining: 6 * SAR })).toBe(6 * SAR);
    expect(due(50 * SAR, { remaining: 2 * SAR })).toBe(2 * SAR);
    expect(due(100 * SAR, { remaining: 1_234_567 })).toBe(1_230_000);
    expect(due(100 * SAR, { remaining: 0 })).toBe(0);
  });

  it("stops at the purchase's credit and stays exact past 2^53", () => {
    expect(due(150 * SAR)).toBe(10 * SAR);
    // SAR 9 billion of credit with a SAR 900 million bonus, two thirds refunded
    const purchase = 9_000_000_000 * SAR;
    const bonus = 900_000_000 * SAR;
    expect(
      due(6_000_000_000 * SAR, { purchase, bonus, remaining: bonus }),
    ).toBe(600_000_000 * SAR);
  });
});
