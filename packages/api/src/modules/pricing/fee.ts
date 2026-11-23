/**
 * The fee added to a top-up payment (docs/platform/adr/0010-admin-controlled-pricing.md):
 * the larger of the percentage and the minimum, rounded to whole cents because that
 * is what a card is charged in. The fee is paid on top; it is never taken from the
 * credits bought (ADR 0003).
 */

export interface TopupFeeRates {
  /** 100 bps = 1%. */
  topupFeeBps: number;
  /** micro-USD (1 USD = 1,000,000). */
  topupFeeMinMicroUsd: number;
}

const MICRO_USD_PER_CENT = 10_000n;

/** Fee in micro-USD, always a whole number of cents. `amount` is the credit bought, in micro-USD. */
export const computeTopupFee = (
  amountMicroUsd: number,
  { topupFeeBps, topupFeeMinMicroUsd }: TopupFeeRates,
): number => {
  if (!Number.isSafeInteger(amountMicroUsd) || amountMicroUsd <= 0) {
    throw new Error(
      `A top-up must be a positive whole number of micro-USD, got ${amountMicroUsd}`,
    );
  }
  if (
    !Number.isSafeInteger(topupFeeBps) ||
    topupFeeBps < 0 ||
    !Number.isSafeInteger(topupFeeMinMicroUsd) ||
    topupFeeMinMicroUsd < 0
  ) {
    throw new Error("Top-up fee rates must be non-negative whole numbers");
  }

  // exact integer arithmetic: amount × bps / 10,000 micro-USD, in cents, half up
  const numerator = BigInt(amountMicroUsd) * BigInt(topupFeeBps);
  const denominator = 10_000n * MICRO_USD_PER_CENT;
  const percentCents = (2n * numerator + denominator) / (2n * denominator);
  const minimumCents =
    (BigInt(topupFeeMinMicroUsd) + MICRO_USD_PER_CENT - 1n) /
    MICRO_USD_PER_CENT;

  const cents = percentCents > minimumCents ? percentCents : minimumCents;
  return Number(cents * MICRO_USD_PER_CENT);
};
