import { describe, expect, it } from "vitest";

import {
  checkPromoCode,
  computeBonus,
  computeFeeDiscount,
  computeTopUpEffect,
  isPromoCodeFormat,
  normalizePromoCode,
  promoCodeStatus,
} from "../math";
import { savePromoCodeSchema } from "../schema";

import type { PromoCheckInput, PromoCodeTerms } from "../math";

const SAR = 1_000_000;
const HALALA = 10_000;
const NOW = new Date("2026-11-27T12:00:00Z");

const terms = (overrides: Partial<PromoCodeTerms> = {}): PromoCodeTerms => ({
  kind: "fee_discount",
  feeDiscountBps: 5000,
  feeDiscountMicroSar: null,
  bonusBps: null,
  bonusCapMicroSar: null,
  creditMicroSar: null,
  maxUses: null,
  uses: 0,
  onePerOrganization: true,
  startsAt: null,
  endsAt: null,
  minTopupMicroSar: null,
  allOrganizations: true,
  isPaused: false,
  ...overrides,
});

const check = (overrides: Partial<PromoCheckInput> = {}) =>
  checkPromoCode({
    code: terms(),
    at: NOW,
    use: { type: "top_up", creditMicroSar: 100 * SAR },
    isListed: false,
    hasRedeemed: false,
    ...overrides,
  });

describe("code normalization", () => {
  it.each([
    ["  welcome-10 ", "WELCOME-10"],
    ["Ramadan_2026", "RAMADAN_2026"],
    ["abc", "ABC"],
  ])("normalizes %j to %j", (raw, normalized) => {
    expect(normalizePromoCode(raw)).toBe(normalized);
    expect(isPromoCodeFormat(normalizePromoCode(raw))).toBe(true);
  });

  it.each(["AB", "A".repeat(33), "WEL COME", "WELCOME!", "ÉTÉ", "", "a.b.c"])(
    "refuses %j",
    (raw) => {
      expect(isPromoCodeFormat(normalizePromoCode(raw))).toBe(false);
    },
  );

  it("accepts the bounds of the length", () => {
    expect(isPromoCodeFormat("ABC")).toBe(true);
    expect(isPromoCodeFormat("A".repeat(32))).toBe(true);
  });

  it("stores the normalized code from what staff type", () => {
    const parsed = savePromoCodeSchema.parse({
      code: " spring-sale ",
      kind: "free_credit",
      feeDiscountBps: null,
      feeDiscountMicroSar: null,
      bonusBps: null,
      bonusCapMicroSar: null,
      creditMicroSar: 10 * SAR,
      maxUses: null,
      onePerOrganization: true,
      startsAt: null,
      endsAt: null,
      minTopupMicroSar: null,
      allOrganizations: true,
    });
    expect(parsed.code).toBe("SPRING-SALE");
  });
});

describe("fee discount", () => {
  it("takes a percentage of the fee, rounded half up to the halala", () => {
    // 50% of SAR 5.50 = SAR 2.75
    expect(computeFeeDiscount(5_500_000, terms())).toBe(2_750_000);
    // 50% of SAR 3.01 = 150.5 halalas, half up to 151
    expect(computeFeeDiscount(3_010_000, terms())).toBe(151 * HALALA);
    // 33.33% of SAR 3.00 = 99.99 halalas, to 100
    expect(computeFeeDiscount(3 * SAR, terms({ feeDiscountBps: 3333 }))).toBe(
      100 * HALALA,
    );
  });

  it("waives the whole fee at 100%, and never more", () => {
    expect(
      computeFeeDiscount(6_880_000, terms({ feeDiscountBps: 10_000 })),
    ).toBe(6_880_000);
  });

  it("takes a fixed amount off, never below zero", () => {
    const fixed = terms({ feeDiscountBps: null, feeDiscountMicroSar: 2 * SAR });
    expect(computeFeeDiscount(5_500_000, fixed)).toBe(2 * SAR);
    // a SAR 2 discount on a SAR 1.50 fee takes SAR 1.50
    expect(computeFeeDiscount(1_500_000, fixed)).toBe(1_500_000);
    expect(computeFeeDiscount(0, fixed)).toBe(0);
  });

  it("is zero for the other kinds", () => {
    expect(
      computeFeeDiscount(
        5 * SAR,
        terms({ kind: "topup_bonus", feeDiscountBps: 5000 }),
      ),
    ).toBe(0);
  });

  it("refuses a fee that is not whole halalas", () => {
    expect(() => computeFeeDiscount(5_500_001, terms())).toThrow(/halalas/);
  });
});

describe("top-up bonus", () => {
  const bonus = terms({
    kind: "topup_bonus",
    feeDiscountBps: null,
    bonusBps: 1000,
    bonusCapMicroSar: 50 * SAR,
  });

  it("adds a percentage of the credit bought", () => {
    // 10% of SAR 100
    expect(computeBonus(100 * SAR, bonus)).toBe(10 * SAR);
  });

  it("rounds down to the halala", () => {
    // 10% of SAR 20.05 = 200.5 halalas, down to 200
    expect(computeBonus(20_050_000, bonus)).toBe(200 * HALALA);
  });

  it("stops at the cap", () => {
    // 10% of SAR 1,000 is SAR 100, capped at SAR 50
    expect(computeBonus(1000 * SAR, bonus)).toBe(50 * SAR);
    expect(computeBonus(500 * SAR, bonus)).toBe(50 * SAR);
  });

  it("is zero for the other kinds", () => {
    expect(computeBonus(100 * SAR, terms())).toBe(0);
  });

  it("refuses a code that would change nothing", () => {
    // 0.01% of SAR 20 is 0.2 halalas: nothing
    expect(
      computeTopUpEffect(terms({ ...bonus, bonusBps: 1 }), 20 * SAR, 3 * SAR),
    ).toEqual({ refusal: "no_effect" });
    // nothing to take off a fee of zero
    expect(computeTopUpEffect(terms(), 20 * SAR, 0)).toEqual({
      refusal: "no_effect",
    });
    expect(computeTopUpEffect(bonus, 100 * SAR, 5_500_000)).toEqual({
      feeDiscountMicroSar: 0,
      bonusMicroSar: 10 * SAR,
    });
  });
});

describe("refusals", () => {
  it("accepts a code with nothing against it", () => {
    expect(check()).toBeNull();
  });

  it("refuses a paused code", () => {
    expect(check({ code: terms({ isPaused: true }) })).toBe("paused");
  });

  it("refuses a code before its start, and takes it from the start", () => {
    expect(
      check({ code: terms({ startsAt: new Date(NOW.getTime() + 1) }) }),
    ).toBe("not_started");
    expect(check({ code: terms({ startsAt: NOW }) })).toBeNull();
  });

  it("refuses a code from its end", () => {
    expect(check({ code: terms({ endsAt: NOW }) })).toBe("expired");
    expect(
      check({ code: terms({ endsAt: new Date(NOW.getTime() + 1) }) }),
    ).toBeNull();
  });

  it("refuses free credit on a top-up, and a top-up code without one", () => {
    const free = terms({
      kind: "free_credit",
      feeDiscountBps: null,
      creditMicroSar: SAR,
    });
    expect(check({ code: free })).toBe("wrong_kind");
    expect(check({ use: { type: "free_credit" } })).toBe("wrong_kind");
    expect(check({ code: free, use: { type: "free_credit" } })).toBeNull();
  });

  it("refuses an organization that is not on the code's list", () => {
    const listed = terms({ allOrganizations: false });
    expect(check({ code: listed })).toBe("not_for_organization");
    expect(check({ code: listed, isListed: true })).toBeNull();
  });

  it("refuses a code whose uses are all taken", () => {
    expect(check({ code: terms({ maxUses: 3, uses: 3 }) })).toBe("used_up");
    expect(check({ code: terms({ maxUses: 3, uses: 2 }) })).toBeNull();
  });

  it("refuses a second use by one organization, unless the code allows it", () => {
    expect(check({ hasRedeemed: true })).toBe("already_used");
    expect(
      check({ code: terms({ onePerOrganization: false }), hasRedeemed: true }),
    ).toBeNull();
  });

  it("refuses a top-up under the code's minimum", () => {
    const minimum = terms({ minTopupMicroSar: 100 * SAR });
    expect(
      check({
        code: minimum,
        use: { type: "top_up", creditMicroSar: 100 * SAR - HALALA },
      }),
    ).toBe("below_minimum");
    expect(check({ code: minimum })).toBeNull();
  });
});

describe("status in the list", () => {
  it.each([
    [{}, "active"],
    [{ isPaused: true, endsAt: NOW }, "paused"],
    [{ endsAt: NOW }, "expired"],
    [{ maxUses: 1, uses: 1 }, "used_up"],
    [{ startsAt: new Date(NOW.getTime() + 1000) }, "scheduled"],
  ] as const)("%j is %s", (overrides, status) => {
    expect(promoCodeStatus(terms(overrides), NOW)).toBe(status);
  });
});

describe("what staff may save", () => {
  const base = {
    code: "SAVE10",
    kind: "fee_discount" as const,
    feeDiscountBps: 1000,
    feeDiscountMicroSar: null,
    bonusBps: null,
    bonusCapMicroSar: null,
    creditMicroSar: null,
    maxUses: null,
    onePerOrganization: true,
    startsAt: null,
    endsAt: null,
    minTopupMicroSar: null,
    allOrganizations: true,
  };

  it("accepts a complete code of each kind", () => {
    expect(savePromoCodeSchema.safeParse(base).success).toBe(true);
  });

  it.each([
    ["both a percentage and a fixed discount", { feeDiscountMicroSar: SAR }],
    ["neither", { feeDiscountBps: null }],
    ["a bonus on a fee discount", { bonusBps: 100, bonusCapMicroSar: SAR }],
    [
      "a fixed amount in part halalas",
      { feeDiscountBps: null, feeDiscountMicroSar: 1 },
    ],
    ["a percentage over 100%", { feeDiscountBps: 10_001 }],
    [
      "an end before the start",
      { startsAt: NOW, endsAt: new Date(NOW.getTime() - 1) },
    ],
    ["a list with no organization", { allOrganizations: false }],
    [
      "a bonus without a cap",
      { kind: "topup_bonus", feeDiscountBps: null, bonusBps: 1000 },
    ],
    [
      "free credit with a minimum top-up",
      {
        kind: "free_credit",
        feeDiscountBps: null,
        creditMicroSar: SAR,
        minTopupMicroSar: SAR,
      },
    ],
    ["a code with a space", { code: "SAVE 10" }],
  ])("refuses %s", (_label, overrides) => {
    expect(
      savePromoCodeSchema.safeParse({ ...base, ...overrides }).success,
    ).toBe(false);
  });
});
