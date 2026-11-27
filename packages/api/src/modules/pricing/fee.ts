/**
 * The fee added to a top-up payment (docs/platform/adr/0010-admin-controlled-pricing.md):
 * the larger of the percentage and the minimum, rounded to whole halalas because that
 * is what a card is charged in. The fee is paid on top; it is never taken from the
 * credits bought (ADR 0003).
 */

export interface TopupFeeRates {
  /** 100 bps = 1%. */
  topupFeeBps: number;
  /** micro-SAR (1 SAR = 1,000,000). */
  topupFeeMinMicroSar: number;
}

const MICRO_SAR_PER_HALALA = 10_000n;

/** Fee in micro-SAR, always a whole number of halalas. `amount` is the credit bought, in micro-SAR. */
export const computeTopupFee = (
  amountMicroSar: number,
  { topupFeeBps, topupFeeMinMicroSar }: TopupFeeRates,
): number => {
  if (!Number.isSafeInteger(amountMicroSar) || amountMicroSar <= 0) {
    throw new Error(
      `A top-up must be a positive whole number of micro-SAR, got ${amountMicroSar}`,
    );
  }
  if (
    !Number.isSafeInteger(topupFeeBps) ||
    topupFeeBps < 0 ||
    !Number.isSafeInteger(topupFeeMinMicroSar) ||
    topupFeeMinMicroSar < 0
  ) {
    throw new Error("Top-up fee rates must be non-negative whole numbers");
  }

  // exact integer arithmetic: amount × bps / 10,000 micro-SAR, in halalas, half up
  const numerator = BigInt(amountMicroSar) * BigInt(topupFeeBps);
  const denominator = 10_000n * MICRO_SAR_PER_HALALA;
  const percentHalalas = (2n * numerator + denominator) / (2n * denominator);
  const minimumHalalas =
    (BigInt(topupFeeMinMicroSar) + MICRO_SAR_PER_HALALA - 1n) /
    MICRO_SAR_PER_HALALA;

  const halalas =
    percentHalalas > minimumHalalas ? percentHalalas : minimumHalalas;
  return Number(halalas * MICRO_SAR_PER_HALALA);
};
