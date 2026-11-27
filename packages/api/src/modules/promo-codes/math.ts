import { PROMO_CODE_PATTERN } from "@workspace/db/schema";
import { MICRO_SAR_PER_HALALA } from "@workspace/shared/utils";

import type { PromoCodeKind } from "@workspace/db/schema";

/**
 * The rules of a promo code (BILL-08), pure so each one is tested on its own:
 * how a code is written, what it takes off a fee or adds to a top-up, and
 * why it is refused. Money is micro-SAR (ADR 0017); a fee and a top-up are
 * whole halalas, and so is everything worked out here.
 */

/** Why a code cannot be used, in the order they are checked. */
export const PROMO_REFUSALS = [
  "unknown",
  "paused",
  "not_started",
  "expired",
  "wrong_kind",
  "not_for_organization",
  "used_up",
  "already_used",
  "below_minimum",
  "no_effect",
] as const;
export type PromoRefusal = (typeof PROMO_REFUSALS)[number];

/** Trimmed and upper case: codes are case-insensitive and stored this way. */
export const normalizePromoCode = (raw: string) => raw.trim().toUpperCase();

/** A–Z, 0–9, hyphen and underscore, 3 to 32 long, once normalized. */
export const isPromoCodeFormat = (code: string) =>
  PROMO_CODE_PATTERN.test(code);

/** What a code says, as the rules need it. */
export interface PromoCodeTerms {
  kind: PromoCodeKind;
  feeDiscountBps: number | null;
  feeDiscountMicroSar: number | null;
  bonusBps: number | null;
  bonusCapMicroSar: number | null;
  creditMicroSar: number | null;
  maxUses: number | null;
  uses: number;
  onePerOrganization: boolean;
  startsAt: Date | null;
  endsAt: Date | null;
  minTopupMicroSar: number | null;
  allOrganizations: boolean;
  isPaused: boolean;
}

export interface PromoCheckInput {
  code: PromoCodeTerms;
  at: Date;
  /** A top-up, with the credit it buys, or a redemption for free credit. */
  use: { type: "top_up"; creditMicroSar: number } | { type: "free_credit" };
  /** Whether the organization is on the code's list (ignored for all organizations). */
  isListed: boolean;
  /** Whether the organization has used this code before. */
  hasRedeemed: boolean;
}

/** The first reason the code cannot be used now, or null when it can. */
export const checkPromoCode = ({
  code,
  at,
  use,
  isListed,
  hasRedeemed,
}: PromoCheckInput): PromoRefusal | null => {
  const time = at.getTime();
  if (code.isPaused) {
    return "paused";
  }
  if (code.startsAt && time < code.startsAt.getTime()) {
    return "not_started";
  }
  if (code.endsAt && time >= code.endsAt.getTime()) {
    return "expired";
  }
  const isFreeCredit = code.kind === "free_credit";
  if (isFreeCredit !== (use.type === "free_credit")) {
    return "wrong_kind";
  }
  if (!code.allOrganizations && !isListed) {
    return "not_for_organization";
  }
  if (code.maxUses !== null && code.uses >= code.maxUses) {
    return "used_up";
  }
  if (code.onePerOrganization && hasRedeemed) {
    return "already_used";
  }
  if (
    use.type === "top_up" &&
    code.minTopupMicroSar !== null &&
    use.creditMicroSar < code.minTopupMicroSar
  ) {
    return "below_minimum";
  }
  return null;
};

const toHalalas = (microSar: number) => {
  if (
    !Number.isSafeInteger(microSar) ||
    microSar < 0 ||
    microSar % MICRO_SAR_PER_HALALA !== 0
  ) {
    throw new Error(
      `${microSar} micro-SAR is not a whole, non-negative number of halalas`,
    );
  }
  return BigInt(microSar / MICRO_SAR_PER_HALALA);
};
const fromHalalas = (halalas: bigint) =>
  Number(halalas * BigInt(MICRO_SAR_PER_HALALA));
const min = (a: number, b: number) => (a < b ? a : b);

/**
 * What a fee-discount code takes off a fee: a percentage of the fee, rounded
 * half up to the halala as the fee itself is, or a fixed amount; never more
 * than the fee, so the fee never goes below zero. 0 for any other kind.
 */
export const computeFeeDiscount = (
  feeMicroSar: number,
  code: Pick<PromoCodeTerms, "kind" | "feeDiscountBps" | "feeDiscountMicroSar">,
) => {
  const fee = toHalalas(feeMicroSar);
  if (code.kind !== "fee_discount") {
    return 0;
  }
  if (code.feeDiscountBps !== null) {
    const off = (fee * BigInt(code.feeDiscountBps) + 5000n) / 10_000n;
    return fromHalalas(off < fee ? off : fee);
  }
  return min(code.feeDiscountMicroSar ?? 0, fromHalalas(fee));
};

/**
 * The bonus a bonus code adds to a top-up: a percentage of the credit bought,
 * rounded down to the halala, and at most the cap. 0 for any other kind.
 */
export const computeBonus = (
  creditMicroSar: number,
  code: Pick<PromoCodeTerms, "kind" | "bonusBps" | "bonusCapMicroSar">,
) => {
  const credit = toHalalas(creditMicroSar);
  if (code.kind !== "topup_bonus" || code.bonusBps === null) {
    return 0;
  }
  const bonus = fromHalalas((credit * BigInt(code.bonusBps)) / 10_000n);
  return min(bonus, code.bonusCapMicroSar ?? bonus);
};

/** The effect of a top-up code, or why it has none. */
export type TopUpPromoEffect =
  | { feeDiscountMicroSar: number; bonusMicroSar: number }
  | { refusal: "no_effect" };

/** A code that would change nothing on this top-up (a fee of zero, a tiny bonus) is refused, so no use is spent on it. */
export const computeTopUpEffect = (
  code: PromoCodeTerms,
  creditMicroSar: number,
  feeMicroSar: number,
): TopUpPromoEffect => {
  const feeDiscountMicroSar = computeFeeDiscount(feeMicroSar, code);
  const bonusMicroSar = computeBonus(creditMicroSar, code);
  return feeDiscountMicroSar === 0 && bonusMicroSar === 0
    ? { refusal: "no_effect" }
    : { feeDiscountMicroSar, bonusMicroSar };
};

/** How staff see a code in the list. */
export const PROMO_CODE_STATUSES = [
  "active",
  "paused",
  "scheduled",
  "expired",
  "used_up",
] as const;
export type PromoCodeStatus = (typeof PROMO_CODE_STATUSES)[number];

export const promoCodeStatus = (
  code: Pick<
    PromoCodeTerms,
    "isPaused" | "startsAt" | "endsAt" | "maxUses" | "uses"
  >,
  at: Date,
): PromoCodeStatus => {
  if (code.isPaused) {
    return "paused";
  }
  if (code.endsAt && at.getTime() >= code.endsAt.getTime()) {
    return "expired";
  }
  if (code.maxUses !== null && code.uses >= code.maxUses) {
    return "used_up";
  }
  if (code.startsAt && at.getTime() < code.startsAt.getTime()) {
    return "scheduled";
  }
  return "active";
};
