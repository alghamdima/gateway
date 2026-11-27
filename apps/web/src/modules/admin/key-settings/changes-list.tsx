"use client";

import {
  API_KEY_LIMIT_RESETS,
  KEY_EXPIRY_CHOICES,
} from "@workspace/api/schema";
import { useTranslation } from "@workspace/i18n";
import { formatMicroSar } from "@workspace/shared/utils";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@workspace/ui-web/table";

interface KeySettingsChange {
  id: number;
  before: Record<string, unknown>;
  after: Record<string, unknown>;
  changedAt: string;
  changedByName: string | null;
  changedByEmail: string | null;
}

/** Each recorded value as staff read it: riyals, lifetimes and resets in words. */
const FIELDS = [
  "defaultLimitMicroSar",
  "defaultLimitReset",
  "defaultExpiry",
  "maxLifetime",
  "limitPresetsMicroSar",
] as const;

export const ChangesList = ({
  changes,
}: {
  readonly changes: KeySettingsChange[];
}) => {
  const { t, i18n } = useTranslation(["common", "admin", "organization"]);

  const show = (field: (typeof FIELDS)[number], value: unknown) => {
    if (Array.isArray(value)) {
      return value
        .filter((item): item is number => typeof item === "number")
        .map((item) => formatMicroSar(item, i18n.language))
        .join(" / ");
    }
    if (field === "defaultLimitMicroSar") {
      return typeof value === "number"
        ? formatMicroSar(value, i18n.language)
        : t("organization:keys.form.limit.none");
    }
    if (field === "defaultLimitReset") {
      const reset = API_KEY_LIMIT_RESETS.find((item) => item === value);
      return t(`organization:keys.form.reset.${reset ?? "never"}`);
    }
    const expiry = KEY_EXPIRY_CHOICES.find((item) => item === value);
    if (expiry) {
      return t(`organization:keys.form.expiry.${expiry}`);
    }
    return field === "maxLifetime"
      ? t("admin:keySettings.form.maxLifetime.none")
      : "";
  };

  const describe = (values: Record<string, unknown>) =>
    FIELDS.filter((field) => field in values)
      .map(
        (field) =>
          `${t(`admin:keySettings.changes.fields.${field}`)}: ${show(field, values[field])}`,
      )
      .join(", ");

  if (changes.length === 0) {
    return (
      <p className="text-muted-foreground text-sm">
        {t("admin:keySettings.changes.empty")}
      </p>
    );
  }

  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>{t("admin:keySettings.changes.when")}</TableHead>
          <TableHead>{t("admin:keySettings.changes.who")}</TableHead>
          <TableHead>{t("admin:keySettings.changes.before")}</TableHead>
          <TableHead>{t("admin:keySettings.changes.after")}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {changes.map((change) => (
          <TableRow key={change.id}>
            <TableCell className="text-muted-foreground whitespace-nowrap">
              {new Date(change.changedAt).toLocaleString(i18n.language)}
            </TableCell>
            <TableCell>
              {change.changedByName ?? change.changedByEmail ?? "—"}
            </TableCell>
            <TableCell className="text-sm whitespace-normal">
              {describe(change.before)}
            </TableCell>
            <TableCell className="text-sm whitespace-normal">
              {describe(change.after)}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
};
