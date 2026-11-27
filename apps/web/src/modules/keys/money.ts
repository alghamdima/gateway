import {
  CURRENCY,
  formatMicroSar as format,
  MICRO_SAR,
} from "@workspace/shared/utils";

/**
 * Amounts are micro-SAR (1 SAR = 1,000,000) everywhere in the API: the
 * customer's currency (docs/platform/adr/0017-sar-ledger.md).
 */
export { MICRO_SAR, sarToMicroSar } from "@workspace/shared/utils";

/** "SAR 1,234.56"; small charges keep up to six decimals, the ledger's precision. */
export const formatMicroSar = (microSar: number, locale?: string) =>
  format(microSar, locale, { maximumFractionDigits: 6 });

/** A key's usage, always with four decimals, as OpenRouter shows it: "SAR 0.0017". */
export const formatUsage = (microSar: number, locale?: string) =>
  new Intl.NumberFormat(locale, {
    style: "currency",
    currency: CURRENCY,
    minimumFractionDigits: 4,
    maximumFractionDigits: 4,
  }).format(microSar / MICRO_SAR);
