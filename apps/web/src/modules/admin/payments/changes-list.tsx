"use client";

import { useTranslation } from "@workspace/i18n";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@workspace/ui-web/table";

import type { PaymentChange } from "~/modules/admin/payments/types";

interface ChangesListProps {
  readonly changes: PaymentChange[];
}

const FIELDS = ["topupsEnabled", "activeProvider", "enabled", "mode"] as const;

/** Every change to the payment settings, with its value before and after (ADMIN-02). */
export const ChangesList = ({ changes }: ChangesListProps) => {
  const { t, i18n } = useTranslation(["common", "admin"]);

  const showValue = (value: unknown) => {
    if (typeof value === "boolean") {
      return t(`admin:payments.changes.${value ? "on" : "off"}`);
    }
    if (value === "test" || value === "live") {
      return t(`admin:payments.mode.${value}`);
    }
    return typeof value === "string" ? value : null;
  };

  const describe = (values: Record<string, unknown> | null) => {
    if (!values) {
      return t("admin:payments.changes.none");
    }
    return FIELDS.flatMap((field) => {
      const shown = showValue(values[field]);
      return shown === null
        ? []
        : [`${t(`admin:payments.changes.fields.${field}`)}: ${shown}`];
    }).join(", ");
  };

  const targetLabel = (target: string) =>
    target === "settings"
      ? t("admin:payments.changes.settings")
      : target.replace(/^provider:/, "");

  if (changes.length === 0) {
    return (
      <p className="text-muted-foreground text-sm">
        {t("admin:payments.changes.empty")}
      </p>
    );
  }

  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>{t("admin:payments.changes.when")}</TableHead>
          <TableHead>{t("admin:payments.changes.who")}</TableHead>
          <TableHead>{t("admin:payments.changes.what")}</TableHead>
          <TableHead>{t("admin:payments.changes.before")}</TableHead>
          <TableHead>{t("admin:payments.changes.after")}</TableHead>
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
            <TableCell>{targetLabel(change.target)}</TableCell>
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
