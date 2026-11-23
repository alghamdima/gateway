/** Amounts are micro-USD (1 USD = 1,000,000) everywhere in the API. */
export const MICRO_USD = 1_000_000;

export const formatMicroUsd = (microUsd: number, locale?: string) =>
  new Intl.NumberFormat(locale, {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: 2,
    maximumFractionDigits: 6,
  }).format(microUsd / MICRO_USD);

/** Dollars typed by a person, to whole micro-USD. */
export const usdToMicroUsd = (usd: number) => Math.round(usd * MICRO_USD);
