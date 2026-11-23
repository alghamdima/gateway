"use client";

import { useInfiniteQuery } from "@tanstack/react-query";

import { useTranslation } from "@workspace/i18n";
import { Button } from "@workspace/ui-web/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui-web/card";
import { Skeleton } from "@workspace/ui-web/skeleton";
import { Spinner } from "@workspace/ui-web/spinner";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@workspace/ui-web/table";

import { admin } from "~/modules/admin/lib/api";

/** The request body when there is one: method and path are already in the action and target. */
const describe = (details: Record<string, unknown> | null) => {
  if (!details) {
    return "";
  }
  return JSON.stringify("body" in details ? details.body : details);
};

export const AuditLog = () => {
  const { t, i18n } = useTranslation(["common", "admin"]);
  const audit = useInfiniteQuery(admin.queries.audit.list());
  const rows = audit.data?.pages.flat() ?? [];

  return (
    <Card className="w-full">
      <CardHeader>
        <CardTitle>{t("admin:audit.header.title")}</CardTitle>
        <CardDescription>{t("admin:audit.header.description")}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {audit.isPending && <Skeleton className="h-40 w-full" />}
        {!audit.isPending && rows.length === 0 && (
          <p className="text-muted-foreground text-sm">
            {t("admin:audit.empty")}
          </p>
        )}
        {rows.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("admin:audit.columns.when")}</TableHead>
                <TableHead>{t("admin:audit.columns.who")}</TableHead>
                <TableHead>{t("admin:audit.columns.action")}</TableHead>
                <TableHead>{t("admin:audit.columns.target")}</TableHead>
                <TableHead>{t("admin:audit.columns.details")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((row) => (
                <TableRow key={row.id}>
                  <TableCell className="text-muted-foreground whitespace-nowrap">
                    {new Date(row.createdAt).toLocaleString(i18n.language)}
                  </TableCell>
                  <TableCell>
                    <div>{row.actorName ?? row.actorEmail ?? row.actorId}</div>
                    {row.actorRole && (
                      <div className="text-muted-foreground text-xs">
                        {row.actorRole}
                      </div>
                    )}
                  </TableCell>
                  <TableCell className="font-mono">{row.action}</TableCell>
                  <TableCell
                    className="max-w-48 truncate font-mono"
                    title={row.targetId ?? undefined}
                  >
                    {[row.targetType, row.targetId].filter(Boolean).join(" ")}
                  </TableCell>
                  <TableCell className="font-mono text-xs break-all whitespace-normal">
                    {describe(row.details)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        {audit.hasNextPage && (
          <Button
            variant="outline"
            onClick={() => void audit.fetchNextPage()}
            disabled={audit.isFetchingNextPage}
          >
            {audit.isFetchingNextPage ? <Spinner /> : t("admin:audit.more")}
          </Button>
        )}
      </CardContent>
    </Card>
  );
};
