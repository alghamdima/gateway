"use client";

import { useQuery } from "@tanstack/react-query";
import { useState } from "react";

import { useTranslation } from "@workspace/i18n";
import { Badge } from "@workspace/ui-web/badge";
import { Button } from "@workspace/ui-web/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui-web/card";
import { Skeleton } from "@workspace/ui-web/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@workspace/ui-web/table";

import { admin } from "~/modules/admin/lib/api";
import { useStaffPermission } from "~/modules/admin/lib/use-staff-permission";
import { ProviderAccounts } from "~/modules/admin/providers/accounts";
import { TermsForm } from "~/modules/admin/providers/terms-form";

import type { CatalogEntry } from "~/modules/admin/providers/terms-form";

export const Providers = () => {
  const { t } = useTranslation(["common", "admin"]);
  const catalog = useQuery(admin.queries.providers.getCatalog());
  const canEdit = useStaffPermission({ providers: ["update"] });
  const [editing, setEditing] = useState<CatalogEntry | null>(null);

  return (
    <div className="@container flex w-full flex-col gap-6">
      <ProviderAccounts />

      <Card>
        <CardHeader>
          <CardTitle>{t("admin:providers.header.title")}</CardTitle>
          <CardDescription>
            {t("admin:providers.header.description")}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {catalog.isError && (
            <p className="text-destructive text-sm">
              {t("admin:providers.unavailable")}
            </p>
          )}
          {catalog.isPending && <Skeleton className="h-32 w-full" />}
          {catalog.data && (
            <Catalog
              entries={catalog.data}
              canEdit={canEdit}
              onEdit={setEditing}
            />
          )}
        </CardContent>
      </Card>

      {editing && (
        <Card>
          <CardHeader>
            <CardTitle>
              {t("admin:providers.form.title", {
                key: editing.name || editing.keyId,
                provider: editing.provider,
              })}
            </CardTitle>
            <CardDescription>
              {t("admin:providers.form.description")}
            </CardDescription>
          </CardHeader>
          <CardContent>
            <TermsForm
              key={editing.keyId}
              entry={editing}
              onDone={() => setEditing(null)}
            />
          </CardContent>
        </Card>
      )}
    </div>
  );
};

interface CatalogProps {
  readonly entries: CatalogEntry[];
  readonly canEdit: boolean;
  readonly onEdit: (entry: CatalogEntry) => void;
}

const Catalog = ({ entries, canEdit, onEdit }: CatalogProps) => {
  const { t } = useTranslation(["common", "admin"]);

  if (entries.length === 0) {
    return (
      <p className="text-muted-foreground text-sm">
        {t("admin:providers.empty")}
      </p>
    );
  }

  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>{t("admin:providers.columns.provider")}</TableHead>
          <TableHead>{t("admin:providers.columns.key")}</TableHead>
          <TableHead>{t("admin:providers.columns.region")}</TableHead>
          <TableHead>{t("admin:providers.columns.retention")}</TableHead>
          <TableHead>{t("admin:providers.columns.training")}</TableHead>
          <TableHead>{t("admin:providers.columns.agreement")}</TableHead>
          <TableHead>{t("admin:providers.columns.verified")}</TableHead>
          <TableHead className="w-0" />
        </TableRow>
      </TableHeader>
      <TableBody>
        {entries.map((entry) => (
          <TableRow key={entry.keyId}>
            <TableCell className="font-mono">{entry.provider}</TableCell>
            <TableCell>
              <div className="flex flex-wrap items-center gap-2">
                <span>{entry.name || entry.keyId}</span>
                <KeyState entry={entry} />
              </div>
            </TableCell>
            {entry.terms ? (
              <>
                <TableCell>
                  {t(`admin:providers.regions.${entry.terms.region}`)}
                </TableCell>
                <TableCell>
                  <Retention days={entry.terms.retentionDays} />
                </TableCell>
                <TableCell>
                  <Training trains={entry.terms.trainsOnData} />
                </TableCell>
                <TableCell>
                  {t(`admin:providers.agreements.${entry.terms.agreement}`)}
                </TableCell>
                <TableCell className="text-muted-foreground whitespace-nowrap">
                  {entry.terms.verifiedOn}
                </TableCell>
              </>
            ) : (
              <TableCell colSpan={5} className="text-muted-foreground">
                {t("admin:providers.noTerms")}
              </TableCell>
            )}
            <TableCell>
              {canEdit && (
                <Button
                  variant="ghost"
                  size="sm"
                  type="button"
                  onClick={() => onEdit(entry)}
                >
                  {t("edit")}
                </Button>
              )}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
};

const KeyState = ({ entry }: { readonly entry: CatalogEntry }) => {
  const { t } = useTranslation("admin");
  if (!entry.isInGateway) {
    return <Badge variant="destructive">{t("providers.state.gone")}</Badge>;
  }
  if (!entry.isEnabled) {
    return <Badge variant="secondary">{t("providers.state.off")}</Badge>;
  }
  return null;
};

const Retention = ({ days }: { readonly days: number | null }) => {
  const { t } = useTranslation("admin");
  if (days === null) {
    return <Badge variant="outline">{t("providers.unknown")}</Badge>;
  }
  return days === 0 ? (
    <Badge>{t("providers.retention.none")}</Badge>
  ) : (
    <span>{t("providers.retention.days", { count: days })}</span>
  );
};

const Training = ({ trains }: { readonly trains: boolean | null }) => {
  const { t } = useTranslation("admin");
  if (trains === null) {
    return <Badge variant="outline">{t("providers.unknown")}</Badge>;
  }
  return trains ? (
    <span>{t("providers.training.yes")}</span>
  ) : (
    <Badge>{t("providers.training.no")}</Badge>
  );
};
