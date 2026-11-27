import { describe, expect, it } from "vitest";

import { attributeLots, expiringSoon } from "../lots";

import type { LotEvent, LotTerms } from "../lots";

// The attribution rule of ADR 0019, without a database.
const SAR = 1_000_000;
const DAY = 86_400_000;
const T0 = new Date("2026-01-01T00:00:00Z").getTime();

const lot = (
  id: string,
  grantedMicroSar: number,
  expiresInDays: number | null,
  source: LotTerms["source"] = "top_up",
  createdDay = 0,
): LotEvent => ({
  type: "grant",
  lot: {
    id,
    source,
    grantedMicroSar,
    expiresAt:
      expiresInDays === null ? null : new Date(T0 + expiresInDays * DAY),
    createdAt: new Date(T0 + createdDay * DAY),
  },
});
const use = (amountMicroSar: number): LotEvent => ({
  type: "debit",
  amountMicroSar,
});
const take = (lotId: string, amountMicroSar: number): LotEvent => ({
  type: "lot_debit",
  lotId,
  amountMicroSar,
});

const left = (events: LotEvent[]) => {
  const { remaining, debtMicroSar } = attributeLots(events);
  return { ...Object.fromEntries(remaining), debt: debtMicroSar };
};

describe("attributeLots", () => {
  it("spends the lot that expires first, across several lots with spending in between", () => {
    expect(
      left([
        lot("a", 100 * SAR, 300),
        use(30 * SAR), // a: 70
        lot("b", 50 * SAR, 30, "top_up", 1), // expires before a
        use(60 * SAR), // b: 0, a: 60
        lot("c", 20 * SAR, null, "adjustment", 2), // never expires: last
        use(65 * SAR), // a: 0, c: 15
      ]),
    ).toEqual({ a: 0, b: 0, c: 15 * SAR, debt: 0 });
  });

  it("on the same expiry, spends promo credit before an adjustment before bought credit, then the older", () => {
    expect(
      left([
        lot("bought", 10 * SAR, 365, "top_up"),
        lot("bonus", 1 * SAR, 365, "promo"),
        lot("staff", 2 * SAR, 365, "adjustment"),
        lot("bought-later", 10 * SAR, 365, "top_up", 1),
        use(4 * SAR),
      ]),
    ).toEqual({
      bonus: 0,
      staff: 0,
      bought: 9 * SAR,
      "bought-later": 10 * SAR,
      debt: 0,
    });
  });

  it("takes a refund or an expiry from the lot it names, and spends any rest like usage", () => {
    expect(
      left([
        lot("a", 10 * SAR, 10),
        lot("b", 10 * SAR, 20),
        use(4 * SAR), // a: 6
        take("b", 3 * SAR), // a refund of b
        take("a", 8 * SAR), // more than a has: 6 from a, 2 from b
      ]),
    ).toEqual({ a: 0, b: 5 * SAR, debt: 0 });
  });

  it("counts spending no lot covers as debt, and a new grant pays it first", () => {
    expect(left([use(5 * SAR), lot("a", 3 * SAR, 10)])).toEqual({
      a: 0,
      debt: 2 * SAR,
    });
    expect(
      left([lot("a", 1 * SAR, 10), use(3 * SAR), lot("b", 10 * SAR, 20)]),
    ).toEqual({ a: 0, b: 8 * SAR, debt: 0 });
  });

  it("keeps the lots' remainders equal to the balance when positive, and zero otherwise", () => {
    // a deterministic walk through grants, spending, refunds and expiries
    let seed = 7;
    const next = (n: number) => {
      seed = (seed * 1_103_515_245 + 12_345) % 2_147_483_648;
      return seed % n;
    };
    const events: LotEvent[] = [];
    let balance = 0;
    const ids: string[] = [];
    for (let step = 0; step < 600; step++) {
      const choice = next(10);
      if (choice < 3) {
        const id = `lot${step}`;
        const granted = (1 + next(50)) * 10_000;
        ids.push(id);
        events.push(
          lot(
            id,
            granted,
            next(4) === 0 ? null : next(400),
            (["top_up", "promo", "adjustment"] as const)[next(3)],
            step,
          ),
        );
        balance += granted;
      } else if (choice < 9 || ids.length === 0) {
        const amount = 1 + next(300_000);
        events.push(use(amount));
        balance -= amount;
      } else {
        const amount = 1 + next(200_000);
        events.push(take(ids[next(ids.length)] ?? "", amount));
        balance -= amount;
      }
      const { remaining, debtMicroSar } = attributeLots(events);
      const sum = [...remaining.values()].reduce((a, b) => a + b, 0);
      expect(sum).toBe(Math.max(balance, 0));
      expect(debtMicroSar).toBe(Math.max(-balance, 0));
      expect([...remaining.values()].every((value) => value >= 0)).toBe(true);
    }
  });
});

describe("expiringSoon", () => {
  const now = new Date(T0);
  const at = (days: number) => new Date(T0 + days * DAY).toISOString();

  it("warns 7 days before, else 30 days before, with the amount and the first date", () => {
    expect(
      expiringSoon(
        [
          { remainingMicroSar: 5 * SAR, expiresAt: at(6) },
          { remainingMicroSar: 2 * SAR, expiresAt: at(3) },
          { remainingMicroSar: 9 * SAR, expiresAt: at(20) },
        ],
        now,
      ),
    ).toEqual({
      withinDays: 7,
      amountMicroSar: 7 * SAR,
      firstExpiresAt: at(3),
    });
    expect(
      expiringSoon([{ remainingMicroSar: 9 * SAR, expiresAt: at(20) }], now),
    ).toEqual({
      withinDays: 30,
      amountMicroSar: 9 * SAR,
      firstExpiresAt: at(20),
    });
  });

  it("says nothing for credit that expires later or never", () => {
    expect(
      expiringSoon(
        [
          { remainingMicroSar: 9 * SAR, expiresAt: at(31) },
          { remainingMicroSar: 9 * SAR, expiresAt: null },
        ],
        now,
      ),
    ).toBeNull();
  });
});
