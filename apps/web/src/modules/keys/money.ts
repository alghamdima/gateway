import { formatMicroSar as format } from "@workspace/shared/utils";

/**
 * Amounts are micro-SAR (1 SAR = 1,000,000) everywhere in the API: the
 * customer's currency (docs/platform/adr/0017-sar-ledger.md).
 */
export { MICRO_SAR, sarToMicroSar } from "@workspace/shared/utils";

/** "SAR 1,234.56"; small charges keep up to six decimals, the ledger's precision. */
export const formatMicroSar = (microSar: number, locale?: string) =>
  format(microSar, locale, { maximumFractionDigits: 6 });
