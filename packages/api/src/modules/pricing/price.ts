/**
 * Unit prices as OpenRouter shows them: decimal strings of USD per token
 * (research brief 03). A per-token price needs more places than micro-SAR
 * hold, so the markup is applied to the exact decimal of the catalog's
 * number, never through floating point: price × (10,000 + bps) / 10,000.
 */
export const marketPrice = (costPerToken: number, markupBps: number): string =>
  scaledPrice(costPerToken, markupBps, 1n, 0);

/**
 * The same price in SAR per token: × the effective USD to SAR rate (× 1,000,000),
 * still exact. The catalog shows it beside the USD price; what a request is
 * charged is converted by the credits plugin and recorded in the ledger.
 */
export const marketPriceSar = (
  costPerToken: number,
  markupBps: number,
  usdToSarRateE6: number,
): string => {
  if (!Number.isSafeInteger(usdToSarRateE6) || usdToSarRateE6 <= 0) {
    throw new Error(
      `A USD to SAR rate must be a positive integer, got ${usdToSarRateE6}`,
    );
  }
  return scaledPrice(costPerToken, markupBps, BigInt(usdToSarRateE6), -6);
};

/** costPerToken × (10,000 + bps) / 10,000 × factor × 10^factorScale, as a plain decimal. */
const scaledPrice = (
  costPerToken: number,
  markupBps: number,
  factor: bigint,
  factorScale: number,
): string => {
  if (!Number.isFinite(costPerToken) || costPerToken < 0) {
    throw new Error(
      `A unit cost must be a non-negative number, got ${costPerToken}`,
    );
  }
  if (!Number.isSafeInteger(markupBps) || markupBps < 0) {
    throw new Error(
      `A markup must be non-negative basis points, got ${markupBps}`,
    );
  }

  // the shortest decimal that reads back as this number, e.g. "1.5e-7"
  const [mantissa = "0", exponent = "0"] = costPerToken
    .toExponential()
    .split("e");
  const [whole = "0", fraction = ""] = mantissa.split(".");
  const digits = BigInt(whole + fraction) * BigInt(10_000 + markupBps) * factor;
  // value = digits × 10^scale
  const scale = Number(exponent) - fraction.length - 4 + factorScale;

  return toPlainDecimal(digits, scale);
};

const toPlainDecimal = (digits: bigint, scale: number): string => {
  if (digits === 0n) {
    return "0";
  }
  const text = digits.toString();
  if (scale >= 0) {
    return text + "0".repeat(scale);
  }
  const places = -scale;
  const padded = text.padStart(places + 1, "0");
  const integer = padded.slice(0, padded.length - places);
  const decimals = padded.slice(padded.length - places).replace(/0+$/, "");
  return decimals ? `${integer}.${decimals}` : integer;
};
