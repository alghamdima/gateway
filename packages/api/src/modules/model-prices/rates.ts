import {
  MAX_TOKEN_RATE_USD,
  MAX_UNIT_RATE_USD,
  MODEL_PRICE_SCALE,
  TYPED_RATE_FIELDS,
} from "@workspace/db/schema";

import { marketPrice } from "../pricing/price";

import type { OtherRates, TypedRateColumn } from "@workspace/db/schema";

/**
 * The rates of one price and the arithmetic on them
 * (docs/platform/adr/0018-model-price-table.md). A rate is USD per unit, kept
 * as an exact decimal string; it becomes a float only in the patch sent to the
 * gateway, which prices in float64.
 */

/**
 * Every unit besides the five typed rates: the gateway's `datasheet.Options`
 * cost fields (apps/gateway/framework/modelcatalog/datasheet/types.go), all of
 * which a pricing override can patch. A test reads that file and fails when
 * this list and the gateway's part. A new unit is a new name here, not a
 * migration.
 */
export const OTHER_RATE_FIELDS = [
  "input_cost_per_token_batches",
  "output_cost_per_token_batches",
  "input_cost_per_token_priority",
  "output_cost_per_token_priority",
  "input_cost_per_token_ultrafast",
  "output_cost_per_token_ultrafast",
  "input_cost_per_token_flex",
  "output_cost_per_token_flex",
  "input_cost_per_token_decisions",
  "output_cost_per_token_decisions",
  "input_cost_per_token_fast",
  "output_cost_per_token_fast",
  "input_cost_per_character",
  "input_cost_per_token_above_128k_tokens",
  "input_cost_per_image_above_128k_tokens",
  "input_cost_per_video_per_second_above_128k_tokens",
  "input_cost_per_audio_per_second_above_128k_tokens",
  "output_cost_per_token_above_128k_tokens",
  "input_cost_per_token_above_100k_tokens",
  "output_cost_per_token_above_100k_tokens",
  "input_cost_per_token_above_200k_tokens",
  "input_cost_per_token_above_200k_tokens_priority",
  "output_cost_per_token_above_200k_tokens",
  "output_cost_per_token_above_200k_tokens_priority",
  "input_cost_per_token_above_272k_tokens",
  "input_cost_per_token_above_272k_tokens_priority",
  "input_cost_per_token_flex_above_272k_tokens",
  "output_cost_per_token_above_272k_tokens",
  "output_cost_per_token_above_272k_tokens_priority",
  "output_cost_per_token_flex_above_272k_tokens",
  "input_cost_per_token_above_272k_tokens_ultrafast",
  "output_cost_per_token_above_272k_tokens_ultrafast",
  "cache_creation_input_token_cost_above_200k_tokens",
  "cache_read_input_token_cost_above_200k_tokens",
  "cache_read_input_token_cost_above_200k_tokens_priority",
  "cache_creation_input_token_cost_above_1hr_above_200k_tokens",
  "cache_creation_input_token_cost_above_100k_tokens",
  "cache_read_input_token_cost_above_100k_tokens",
  "cache_creation_input_token_cost_above_1hr_above_100k_tokens",
  "cache_creation_input_audio_token_cost",
  "cache_read_input_token_cost_priority",
  "cache_read_input_token_cost_ultrafast",
  "cache_read_input_token_cost_flex",
  "cache_read_input_image_token_cost",
  "cache_read_input_token_cost_above_272k_tokens",
  "cache_read_input_token_cost_above_272k_tokens_priority",
  "cache_read_input_token_cost_flex_above_272k_tokens",
  "cache_read_input_token_cost_above_272k_tokens_ultrafast",
  "cache_creation_input_token_cost_above_272k_tokens",
  "cache_creation_input_token_cost_flex",
  "cache_creation_input_token_cost_flex_above_272k_tokens",
  "cache_creation_input_token_cost_priority",
  "cache_creation_input_token_cost_above_272k_tokens_priority",
  "cache_creation_input_token_cost_ultrafast",
  "cache_creation_input_token_cost_above_272k_tokens_ultrafast",
  "cache_creation_input_token_cost_fast",
  "cache_creation_input_token_cost_above_1hr_fast",
  "cache_read_input_token_cost_fast",
  "cache_creation_input_token_cost_above_12hr",
  "cache_creation_input_token_cost_above_12hr_above_272k_tokens",
  "cache_creation_input_token_cost_above_12hr_priority",
  "cache_creation_input_token_cost_above_12hr_above_272k_tokens_priority",
  "cache_creation_input_token_cost_above_12hr_flex",
  "cache_creation_input_token_cost_above_12hr_above_272k_tokens_flex",
  "cache_creation_input_token_cost_above_12hr_ultrafast",
  "cache_creation_input_token_cost_above_12hr_above_272k_tokens_ultrafast",
  "input_cost_per_image",
  "input_cost_per_pixel",
  "output_cost_per_image",
  "output_cost_per_pixel",
  "output_cost_per_image_premium_image",
  "output_cost_per_image_above_512_and_512_pixels",
  "output_cost_per_image_above_512_and_512_pixels_and_premium_image",
  "output_cost_per_image_above_1024_and_1024_pixels",
  "output_cost_per_image_above_1024_and_1024_pixels_and_premium_image",
  "output_cost_per_image_above_1024_and_1536_pixels",
  "output_cost_per_image_above_1536_and_1024_pixels",
  "output_cost_per_image_above_2048_and_2048_pixels",
  "output_cost_per_image_above_4096_and_4096_pixels",
  "output_cost_per_image_above_4_megapixels",
  "output_cost_per_image_above_8_megapixels",
  "output_cost_per_image_above_16_megapixels",
  "output_cost_per_image_above_32_megapixels",
  "output_cost_per_image_above_64_megapixels",
  "output_cost_per_image_low_quality",
  "output_cost_per_image_medium_quality",
  "output_cost_per_image_high_quality",
  "output_cost_per_image_auto_quality",
  "input_cost_per_image_token",
  "output_cost_per_image_token",
  "output_cost_per_image_above_1024_and_1024_pixels_low_quality",
  "output_cost_per_image_above_1024_and_1536_pixels_low_quality",
  "output_cost_per_image_above_1536_and_1024_pixels_low_quality",
  "output_cost_per_image_above_1024_and_1024_pixels_medium_quality",
  "output_cost_per_image_above_1024_and_1536_pixels_medium_quality",
  "output_cost_per_image_above_1536_and_1024_pixels_medium_quality",
  "output_cost_per_image_above_1024_and_1024_pixels_high_quality",
  "output_cost_per_image_above_1024_and_1536_pixels_high_quality",
  "output_cost_per_image_above_1536_and_1024_pixels_high_quality",
  "output_cost_per_image_above_1024_and_1024_pixels_standard_quality",
  "output_cost_per_image_above_1024_and_1536_pixels_standard_quality",
  "output_cost_per_image_above_1536_and_1024_pixels_standard_quality",
  "input_cost_per_audio_token",
  "input_cost_per_audio_per_second",
  "input_cost_per_second",
  "input_cost_per_video_per_second",
  "output_cost_per_audio_token",
  "output_cost_per_video_per_second",
  "output_cost_per_second",
  "input_cost_per_token_voice_live",
  "output_cost_per_token_voice_live",
  "cache_read_input_token_cost_voice_live",
  "input_cost_per_audio_token_voice_live",
  "output_cost_per_audio_token_voice_live",
  "input_cost_per_standard_speech_audio_token_voice_live",
  "output_cost_per_standard_speech_audio_token_voice_live",
  "input_cost_per_custom_speech_audio_token_voice_live",
  "output_cost_per_custom_speech_audio_token_voice_live",
  "output_cost_per_video_per_second_480p",
  "output_cost_per_video_per_second_720p",
  "output_cost_per_video_per_second_1024p",
  "output_cost_per_video_per_second_1080p",
  "output_cost_per_video_per_second_4k",
  "output_cost_per_video_token",
  "search_context_cost_per_query",
  "web_search_cost_per_request",
  "code_interpreter_cost_per_session",
  "input_cost_per_query",
  "inference_geo_us_multiplier",
  "cost_per_request",
  "ocr_cost_per_page",
  "annotation_cost_per_page",
  "off_peak_cost_multiplier",
] as const;
export type OtherRateField = (typeof OTHER_RATE_FIELDS)[number];

const OTHER_RATE_FIELD_SET: ReadonlySet<string> = new Set(OTHER_RATE_FIELDS);
export const isOtherRateField = (field: string): field is OtherRateField =>
  OTHER_RATE_FIELD_SET.has(field);

export const TYPED_RATE_COLUMNS = Object.keys(
  TYPED_RATE_FIELDS,
) as TypedRateColumn[];

/** The rates of one price, suggestion or gateway row, as exact decimals of USD per unit. */
export interface Rates {
  inputUsd: string | null;
  outputUsd: string | null;
  cacheReadUsd: string | null;
  cacheWriteUsd: string | null;
  cacheWrite1hUsd: string | null;
  otherRates: OtherRates;
}

export const NO_RATES: Rates = {
  inputUsd: null,
  outputUsd: null,
  cacheReadUsd: null,
  cacheWriteUsd: null,
  cacheWrite1hUsd: null,
  otherRates: {},
};

const DECIMAL = new RegExp(
  `^(0|[1-9][0-9]{0,5})(\\.[0-9]{1,${MODEL_PRICE_SCALE}})?$`,
);

/** Whether a string is a rate the database can store exactly: no exponent, no sign, at most the scale's decimals. */
export const isDecimalRate = (value: string) => DECIMAL.test(value);

/**
 * The canonical form of a decimal: no trailing zeros after the point and no
 * bare point. Postgres returns numeric(36,30) padded to 30 decimals.
 */
export const normalizeDecimal = (value: string): string => {
  if (!value.includes(".")) {
    return value.replace(/^0+(?=\d)/, "");
  }
  const trimmed = value.replace(/0+$/, "").replace(/\.$/, "");
  return trimmed.replace(/^0+(?=\d)/, "") || "0";
};

/** The exact decimal of a float rate as the gateway holds it: its shortest round-trip digits. */
export const floatToDecimal = (value: number): string => marketPrice(value);

/** A rate is at most the ceiling of its unit: per token for the typed five, per unit otherwise. */
export const rateCeiling = (field: TypedRateColumn | OtherRateField) =>
  field in TYPED_RATE_FIELDS ? MAX_TOKEN_RATE_USD : MAX_UNIT_RATE_USD;

/** Rates with every decimal in canonical form, so equal prices compare equal. */
export const normalizeRates = (rates: Rates): Rates => ({
  inputUsd: rates.inputUsd === null ? null : normalizeDecimal(rates.inputUsd),
  outputUsd:
    rates.outputUsd === null ? null : normalizeDecimal(rates.outputUsd),
  cacheReadUsd:
    rates.cacheReadUsd === null ? null : normalizeDecimal(rates.cacheReadUsd),
  cacheWriteUsd:
    rates.cacheWriteUsd === null ? null : normalizeDecimal(rates.cacheWriteUsd),
  cacheWrite1hUsd:
    rates.cacheWrite1hUsd === null
      ? null
      : normalizeDecimal(rates.cacheWrite1hUsd),
  otherRates: Object.fromEntries(
    Object.entries(rates.otherRates)
      .map(([field, value]) => [field, normalizeDecimal(value)] as const)
      .sort(([a], [b]) => a.localeCompare(b)),
  ),
});

/** Every rate set, by the gateway's field name: the push's patch before it becomes floats. */
export const ratesByField = (rates: Rates): Record<string, string> => {
  const normalized = normalizeRates(rates);
  const fields: Record<string, string> = {};
  for (const column of TYPED_RATE_COLUMNS) {
    const value = normalized[column];
    if (value !== null) {
      fields[TYPED_RATE_FIELDS[column]] = value;
    }
  }
  return { ...fields, ...normalized.otherRates };
};

/** Two sets of rates are the same price: the same fields, each the same decimal. */
export const ratesEqual = (a: Rates, b: Rates): boolean => {
  const left = ratesByField(a);
  const right = ratesByField(b);
  const fields = Object.keys(left);
  return (
    fields.length === Object.keys(right).length &&
    fields.every((field) => right[field] === left[field])
  );
};

/** The pricing override's patch: each rate as the float nearest its decimal. */
export const toPatch = (rates: Rates): Record<string, number> =>
  Object.fromEntries(
    Object.entries(ratesByField(rates)).map(([field, value]) => [
      field,
      Number(value),
    ]),
  );

export interface RatesFromGateway {
  rates: Rates;
  /** Cost fields we could not take: unknown to us, out of range or not exact. */
  skipped: string[];
}

/**
 * The rates of a gateway row (a datasheet entry, keyed by the gateway's field
 * names, values in float USD per unit). Only cost fields the push can send are
 * taken; anything else that looks like a cost is listed as skipped.
 */
export const ratesFromGateway = (
  row: Record<string, unknown>,
): RatesFromGateway => {
  const rates: Rates = { ...NO_RATES, otherRates: {} };
  const skipped: string[] = [];
  const typedByField = new Map<string, TypedRateColumn>(
    TYPED_RATE_COLUMNS.map((column) => [TYPED_RATE_FIELDS[column], column]),
  );
  for (const [field, value] of Object.entries(row)) {
    const column = typedByField.get(field);
    const known = column !== undefined || isOtherRateField(field);
    if (!known) {
      if (/cost|price/.test(field) && typeof value === "number") {
        skipped.push(field);
      }
      continue;
    }
    if (typeof value !== "number") {
      continue;
    }
    const decimal =
      Number.isFinite(value) && value >= 0 ? floatToDecimal(value) : "";
    const ceiling = column ? MAX_TOKEN_RATE_USD : MAX_UNIT_RATE_USD;
    if (!isDecimalRate(decimal) || Number(decimal) > ceiling) {
      skipped.push(field);
      continue;
    }
    if (column) {
      rates[column] = decimal;
    } else {
      rates.otherRates[field] = decimal;
    }
  }
  return { rates: normalizeRates(rates), skipped: skipped.sort() };
};

/** Whether rates price anything at all. */
export const hasAnyRate = (rates: Rates) =>
  Object.keys(ratesByField(rates)).length > 0;

/**
 * decimal × rateE6 / 1,000,000, exact: a USD rate in SAR at an effective
 * USD to SAR rate (ADR 0017). What the credits plugin charges per unit.
 */
export const usdToSar = (usd: string, rateE6: number): string => {
  if (!Number.isSafeInteger(rateE6) || rateE6 <= 0) {
    throw new Error(
      `A USD to SAR rate must be a positive integer, got ${rateE6}`,
    );
  }
  const [whole = "0", fraction = ""] = normalizeDecimal(usd).split(".");
  const digits = BigInt(whole + fraction) * BigInt(rateE6);
  return shiftDecimal(digits, fraction.length + 6);
};

/** digits × 10^-places as a canonical decimal. */
const shiftDecimal = (digits: bigint, places: number): string => {
  const text = digits.toString().padStart(places + 1, "0");
  const integer = text.slice(0, text.length - places);
  const decimals = text.slice(text.length - places);
  return normalizeDecimal(places === 0 ? integer : `${integer}.${decimals}`);
};
