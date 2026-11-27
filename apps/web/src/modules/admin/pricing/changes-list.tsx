"use client";

import { useTranslation } from "@workspace/i18n";
import { formatMicroSar, formatRateE6 } from "@workspace/shared/utils";
import { cn } from "@workspace/ui";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@workspace/ui-web/table";

import { bpsToPercent } from "~/modules/admin/pricing/rates";

interface PricingChange {
  id: number;
  target: string;
  before: Record<string, unknown> | null;
  after: Record<string, unknown> | null;
  changedAt: string;
  changedBy: string;
  changedByName: string | null;
  changedByEmail: string | null;
}

interface ChangesListProps {
  readonly changes: PricingChange[];
}

/**
 * The fields a change shows. Rows recorded before the markup was removed also
 * carry markup fields (and a "provider:<name>" target); the history is
 * append-only, so they stay, and are listed as a setting that no longer exists.
 */
const RATE_KEYS = [
  "topupFeeBps",
  "topupFeeMinMicroSar",
  "topupMinMicroSar",
  "topupMaxMicroSar",
  "topupPresetsMicroSar",
  "usdToSarRateE6",
  "fxSpreadBps",
  "effectiveUsdToSarRateE6",
  // the refund window and the credit expiry (ADMIN-23)
  "refundWindowHours",
  "creditExpiryDays",
] as const;

const sar = (microSar: number) => formatMicroSar(microSar);

/** One recorded value as staff read it: riyals for amounts, decimals for exchange rates, percent for the rest. */
const showValue = (key: (typeof RATE_KEYS)[number], value: unknown) => {
  if (Array.isArray(value)) {
    return value
      .filter((item): item is number => typeof item === "number")
      .map(sar)
      .join(" / ");
  }
  if (typeof value !== "number") {
    return null;
  }
  if (key.endsWith("MicroSar")) {
    return sar(value);
  }
  return key.endsWith("RateE6")
    ? formatRateE6(value)
    : `${bpsToPercent(value)}%`;
};

export const ChangesList = ({ changes }: ChangesListProps) => {
  const { t, i18n } = useTranslation(["common", "admin"]);

  const describe = (values: Record<string, unknown> | null) => {
    if (!values) {
      return t("admin:pricing.changes.none");
    }
    const shown = RATE_KEYS.flatMap((key) => {
      const value = showTerm(key, values) ?? showValue(key, values[key]);
      return value === null
        ? []
        : [`${t(`admin:pricing.changes.fields.${key}`)}: ${value}`];
    });
    return shown.length > 0
      ? shown.join(", ")
      : t("admin:pricing.changes.retired");
  };

  /** The credit terms read in hours and days; no expiry reads "never". */
  const showTerm = (
    key: (typeof RATE_KEYS)[number],
    values: Record<string, unknown>,
  ) => {
    if (!(key in values)) {
      return null;
    }
    const value = values[key];
    if (key === "refundWindowHours" && typeof value === "number") {
      return t("admin:pricing.changes.hours", { count: value });
    }
    if (key === "creditExpiryDays") {
      return typeof value === "number"
        ? t("admin:pricing.changes.days", { count: value })
        : t("admin:pricing.changes.never");
    }
    return null;
  };

  const targetLabel = (target: string) =>
    target === "settings"
      ? t("admin:pricing.changes.settings")
      : target === "credit_terms"
        ? t("admin:pricing.changes.creditTerms")
        : target.replace(/^provider:/, "");

  if (changes.length === 0) {
    return (
      <p className="text-muted-foreground text-sm">
        {t("admin:pricing.changes.empty")}
      </p>
    );
  }

  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>{t("admin:pricing.changes.when")}</TableHead>
          <TableHead>{t("admin:pricing.changes.who")}</TableHead>
          <TableHead>{t("admin:pricing.changes.what")}</TableHead>
          <TableHead>{t("admin:pricing.changes.before")}</TableHead>
          <TableHead>{t("admin:pricing.changes.after")}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {changes.map((change) => (
          <TableRow key={change.id}>
            <TableCell className="text-muted-foreground whitespace-nowrap">
              {new Date(change.changedAt).toLocaleString(i18n.language)}
            </TableCell>
            <TableCell>
              {change.changedByName ??
                change.changedByEmail ??
                change.changedBy}
            </TableCell>
            <TableCell
              className={cn(
                change.target.startsWith("provider:") && "font-mono",
              )}
            >
              {targetLabel(change.target)}
            </TableCell>
            <TableCell className="whitespace-normal">
              {describe(change.before)}
            </TableCell>
            <TableCell className="whitespace-normal">
              {describe(change.after)}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
};
