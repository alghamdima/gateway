"use client";

import { useTranslation } from "@workspace/i18n";
import { cn } from "@workspace/ui";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@workspace/ui-web/table";

import { bpsToPercent, microUsdToUsd } from "~/modules/admin/pricing/rates";

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

const RATE_KEYS = [
  "defaultMarkupBps",
  "topupFeeBps",
  "topupFeeMinMicroUsd",
  "markupBps",
] as const;

export const ChangesList = ({ changes }: ChangesListProps) => {
  const { t, i18n } = useTranslation(["common", "admin"]);

  const describe = (values: Record<string, unknown> | null) => {
    if (!values) {
      return t("admin:pricing.changes.none");
    }
    return RATE_KEYS.filter((key) => typeof values[key] === "number")
      .map((key) => {
        const value = values[key] as number;
        const shown =
          key === "topupFeeMinMicroUsd"
            ? `$${microUsdToUsd(value).toFixed(2)}`
            : `${bpsToPercent(value)}%`;
        return `${t(`admin:pricing.changes.fields.${key}`)}: ${shown}`;
      })
      .join(", ");
  };

  const targetLabel = (target: string) =>
    target === "settings"
      ? t("admin:pricing.changes.settings")
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
              className={cn(change.target !== "settings" && "font-mono")}
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
