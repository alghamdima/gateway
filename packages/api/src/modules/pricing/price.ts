/**
 * Unit prices as OpenRouter shows them: decimal strings of USD per token
 * (research brief 03). A request is charged its provider's cost with no
 * markup (docs/platform/adr/0010-admin-controlled-pricing.md), so the price
 * shown is the catalog's number, written as its exact decimal, never through
 * floating point.
 */
export const marketPrice = (costPerToken: number): string =>
  scaledPrice(costPerToken, 1n, 0);

/**
 * The same price in SAR per token: × the effective USD to SAR rate (× 1,000,000),
 * still exact. This is what the credits plugin charges per token: the
 * provider's cost at the effective rate (ADR 0017).
 */
export const marketPriceSar = (
  costPerToken: number,
  usdToSarRateE6: number,
): string => {
  if (!Number.isSafeInteger(usdToSarRateE6) || usdToSarRateE6 <= 0) {
    throw new Error(
      `A USD to SAR rate must be a positive integer, got ${usdToSarRateE6}`,
    );
  }
  return scaledPrice(costPerToken, BigInt(usdToSarRateE6), -6);
};

/** costPerToken × factor × 10^factorScale, as a plain decimal. */
const scaledPrice = (
  costPerToken: number,
  factor: bigint,
  factorScale: number,
): string => {
  if (!Number.isFinite(costPerToken) || costPerToken < 0) {
    throw new Error(
      `A unit cost must be a non-negative number, got ${costPerToken}`,
    );
  }

  // the shortest decimal that reads back as this number, e.g. "1.5e-7"
  const [mantissa = "0", exponent = "0"] = costPerToken
    .toExponential()
    .split("e");
  const [whole = "0", fraction = ""] = mantissa.split(".");
  const digits = BigInt(whole + fraction) * factor;
  // value = digits × 10^scale
  const scale = Number(exponent) - fraction.length + factorScale;

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
