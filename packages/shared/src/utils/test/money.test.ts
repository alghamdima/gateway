import { describe, expect, it } from "vitest";

import {
  effectiveUsdToSarRateE6,
  formatMicroSar,
  formatRateE6,
  formatUsd,
  sarToMicroSar,
} from "../money";

// Intl separates the code from the amount with a no-break space
const plain = (text: string) => text.replace(/\s/g, " ");

describe("money", () => {
  it("formats micro-SAR as riyals with the currency code", () => {
    expect(plain(formatMicroSar(1_234_560_000))).toBe("SAR 1,234.56");
    expect(plain(formatMicroSar(0))).toBe("SAR 0.00");
    expect(plain(formatMicroSar(-3_000_000))).toBe("-SAR 3.00");
    // a small charge keeps the ledger's precision when asked
    expect(
      plain(formatMicroSar(1_772, "en", { maximumFractionDigits: 6 })),
    ).toBe("SAR 0.001772");
  });

  it("formats USD beside it", () => {
    expect(formatUsd(2)).toBe("$2.00");
  });

  it("turns typed riyals into whole micro-SAR", () => {
    expect(sarToMicroSar(20)).toBe(20_000_000);
    expect(sarToMicroSar(0.1 + 0.2)).toBe(300_000);
  });

  it("adds the bank's spread to the official rate, rounding up", () => {
    expect(effectiveUsdToSarRateE6(3_750_000, 0)).toBe(3_750_000);
    expect(effectiveUsdToSarRateE6(3_750_000, 250)).toBe(3_843_750);
    expect(effectiveUsdToSarRateE6(3_750_001, 1)).toBe(3_750_377); // 3,750,376.0001
    expect(() => effectiveUsdToSarRateE6(0, 0)).toThrow("positive integer");
    expect(() => effectiveUsdToSarRateE6(3_750_000, -1)).toThrow(
      "non-negative",
    );
  });

  it("shows a rate as a plain decimal", () => {
    expect(formatRateE6(3_750_000)).toBe("3.75");
    expect(formatRateE6(3_843_750)).toBe("3.84375");
    expect(formatRateE6(4_000_000)).toBe("4.00");
  });
});
