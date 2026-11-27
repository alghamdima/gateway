"use client";

import { useTranslation } from "@workspace/i18n";
import { formatMicroSar } from "@workspace/shared/utils";

import { bpsToPercent } from "~/modules/admin/pricing/rates";

/** A promo code as the admin API returns it (BILL-08). */
export interface PromoCode {
  id: string;
  code: string;
  kind: "fee_discount" | "topup_bonus" | "free_credit";
  feeDiscountBps: number | null;
  feeDiscountMicroSar: number | null;
  bonusBps: number | null;
  bonusCapMicroSar: number | null;
  creditMicroSar: number | null;
  maxUses: number | null;
  uses: number;
  onePerOrganization: boolean;
  startsAt: string | null;
  endsAt: string | null;
  minTopupMicroSar: number | null;
  allOrganizations: boolean;
  isPaused: boolean;
  note: string;
  status: "active" | "paused" | "scheduled" | "expired" | "used_up";
}

export const STATUS_VARIANT = {
  active: "success",
  paused: "secondary",
  scheduled: "outline",
  expired: "outline",
  used_up: "outline",
} as const;

/** How staff read a code's terms: what it gives, when it is valid, how often it was used. */
export const usePromoCodeFormat = () => {
  const { t, i18n } = useTranslation(["common", "admin"]);
  const locale = i18n.language;
  const money = (microSar: number) => formatMicroSar(microSar, locale);
  const percent = (bps: number) =>
    new Intl.NumberFormat(locale, { maximumFractionDigits: 2 }).format(
      bpsToPercent(bps),
    );
  // an end is the start of the day after the last valid one
  const day = (value: string, isEnd: boolean) =>
    new Intl.DateTimeFormat(locale, { dateStyle: "medium" }).format(
      new Date(new Date(value).getTime() - (isEnd ? 1 : 0)),
    );

  const value = (code: PromoCode) => {
    if (code.kind === "fee_discount") {
      return code.feeDiscountBps === null
        ? t("admin:promoCodes.value.amountOff", {
            amount: money(code.feeDiscountMicroSar ?? 0),
          })
        : t("admin:promoCodes.value.percentOff", {
            percent: percent(code.feeDiscountBps),
          });
    }
    if (code.kind === "topup_bonus") {
      return t("admin:promoCodes.value.bonus", {
        percent: percent(code.bonusBps ?? 0),
        cap: money(code.bonusCapMicroSar ?? 0),
      });
    }
    return t("admin:promoCodes.value.credit", {
      amount: money(code.creditMicroSar ?? 0),
    });
  };

  const validity = (code: Pick<PromoCode, "startsAt" | "endsAt">) => {
    const from = code.startsAt && day(code.startsAt, false);
    const until = code.endsAt && day(code.endsAt, true);
    if (from && until) {
      return t("admin:promoCodes.list.between", { from, until });
    }
    if (from) {
      return t("admin:promoCodes.list.from", { date: from });
    }
    if (until) {
      return t("admin:promoCodes.list.until", { date: until });
    }
    return t("admin:promoCodes.list.always");
  };

  const uses = (code: Pick<PromoCode, "uses" | "maxUses">) =>
    `${code.uses} / ${code.maxUses === null ? t("admin:promoCodes.list.unlimited") : code.maxUses}`;

  return { money, value, validity, uses };
};
