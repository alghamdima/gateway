/**
 * Money as the product shows it (docs/platform/adr/0017-sar-ledger.md). The
 * customer deals in SAR: every balance, hold, limit, top-up and charge is an
 * integer of micro-SAR (1 SAR = 1,000,000). Provider costs and model prices
 * are USD; they become SAR once per request, at the effective USD to SAR rate
 * staff set in the admin (the official rate plus the bank's spread).
 */

export const CURRENCY = "SAR";

/** micro-units per unit: 1 SAR = 1,000,000 micro-SAR, 1 USD = 1,000,000 micro-USD. */
export const MICRO_SAR = 1_000_000;
export const MICRO_USD = 1_000_000;

/** SAR's minor unit, the halala, is 1/100 SAR: what a card is charged in. */
export const MICRO_SAR_PER_HALALA = 10_000;

/** Rates are stored × 1,000,000 as exact integers: 3.75 is 3,750,000. */
export const RATE_E6 = 1_000_000;

/**
 * The rate every hold and charge converts at: the official rate plus the
 * bank's spread, rate × (10,000 + spread) / 10,000, rounded up to the next
 * millionth so the commission is never under-counted. The credits plugin
 * computes the same (apps/gateway/plugins/credits/money.go), and a database
 * check holds every hold and charge to it.
 */
export const effectiveUsdToSarRateE6 = (
  rateE6: number,
  spreadBps: number,
): number => {
  if (!Number.isSafeInteger(rateE6) || rateE6 <= 0) {
    throw new Error(
      `A USD to SAR rate must be a positive integer, got ${rateE6}`,
    );
  }
  if (!Number.isSafeInteger(spreadBps) || spreadBps < 0) {
    throw new Error(`An FX spread must be non-negative bps, got ${spreadBps}`);
  }
  return Number(
    (BigInt(rateE6) * BigInt(10_000 + spreadBps) + 9_999n) / 10_000n,
  );
};

interface FormatOptions {
  /** Defaults to 2; small charges show up to 6, the ledger's precision. */
  maximumFractionDigits?: number;
}

/** "SAR 1,234.56" in English. Arabic follows the locale when it ships. */
export const formatMicroSar = (
  microSar: number,
  locale = "en",
  { maximumFractionDigits = 2 }: FormatOptions = {},
) =>
  new Intl.NumberFormat(locale, {
    style: "currency",
    currency: CURRENCY,
    minimumFractionDigits: 2,
    maximumFractionDigits: Math.max(2, maximumFractionDigits),
  }).format(microSar / MICRO_SAR);

/** A USD amount, for a provider cost or a price shown beside its SAR. */
export const formatUsd = (
  usd: number,
  locale = "en",
  { maximumFractionDigits = 2 }: FormatOptions = {},
) =>
  new Intl.NumberFormat(locale, {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: 2,
    maximumFractionDigits: Math.max(2, maximumFractionDigits),
  }).format(usd);

/** Riyals typed by a person, to whole micro-SAR. */
export const sarToMicroSar = (sar: number) => Math.round(sar * MICRO_SAR);

/** A rate × 1,000,000 as a plain decimal: 3,750,000 is "3.75". */
export const formatRateE6 = (rateE6: number) => {
  const whole = Math.trunc(rateE6 / RATE_E6);
  const fraction = String(rateE6 % RATE_E6)
    .padStart(6, "0")
    .replace(/0+$/, "");
  return fraction ? `${whole}.${fraction.padEnd(2, "0")}` : `${whole}.00`;
};
