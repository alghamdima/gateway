/**
 * Model prices are exact decimals of USD per unit (ADR 0018). Staff read and
 * type token rates per million tokens, as providers publish them; these move
 * the decimal point without passing through floating point.
 */

/** Where a typed rate is stored, and its gateway field name. */
export const TYPED_RATES = [
  { column: "inputUsd", field: "input_cost_per_token" },
  { column: "outputUsd", field: "output_cost_per_token" },
  { column: "cacheReadUsd", field: "cache_read_input_token_cost" },
  { column: "cacheWriteUsd", field: "cache_creation_input_token_cost" },
  {
    column: "cacheWrite1hUsd",
    field: "cache_creation_input_token_cost_above_1hr",
  },
] as const;
export type TypedRateColumn = (typeof TYPED_RATES)[number]["column"];

const PLAIN = /^\d+(\.\d+)?$/;

/** Whether text is a plain non-negative decimal. */
export const isPlainDecimal = (value: string) => PLAIN.test(value.trim());

/** value × 10^places for a plain decimal, as a canonical decimal string. */
export const shiftDecimal = (value: string, places: number): string => {
  const [whole = "0", fraction = ""] = value.trim().split(".");
  const digits = (whole + fraction).replace(/^0+(?=\d)/, "");
  const point = whole.length + places;
  const padded =
    point <= 0 ? "0".repeat(1 - point) + digits : digits.padEnd(point, "0");
  const at = Math.max(point, 1);
  const integer = padded.slice(0, at).replace(/^0+(?=\d)/, "") || "0";
  const decimals = padded.slice(at).replace(/0+$/, "");
  return decimals ? `${integer}.${decimals}` : integer;
};

/** USD per token → USD per million tokens. */
export const perMillion = (perToken: string) => shiftDecimal(perToken, 6);

/** USD per million tokens → USD per token. */
export const perToken = (perMillionTokens: string) =>
  shiftDecimal(perMillionTokens, -6);

/** Whether a gateway field is priced per token (shown per million) or per other unit. */
export const isTokenField = (field: string) =>
  field.includes("token") &&
  !/per_(image|video|audio|pixel|character|query|request|page|second)(?!_token)/.test(
    field,
  );
