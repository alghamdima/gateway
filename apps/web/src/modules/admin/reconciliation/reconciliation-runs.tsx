"use client";

import { useQuery } from "@tanstack/react-query";
import { useState } from "react";

import { handle } from "@workspace/api/utils";
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

import { api } from "~/lib/api/client";

interface Findings {
  organizations?: unknown[];
  reapedHolds?: { holds: number }[];
  unattributed?: { costUsd: number }[];
  error?: string;
}

const STATUS_VARIANT = {
  ok: "outline",
  mismatch: "destructive",
  error: "destructive",
} as const;

/** The daily gateway/ledger comparisons (step 10.1), newest first. */
export const ReconciliationRuns = () => {
  const { t, i18n } = useTranslation(["common", "admin"]);
  const [openId, setOpenId] = useState<string | null>(null);
  const runs = useQuery({
    queryKey: ["admin", "reconciliation"],
    queryFn: () => handle(api.admin.reconciliation.$get)(),
  });
  const usd = new Intl.NumberFormat(i18n.language, {
    style: "currency",
    currency: "USD",
    maximumFractionDigits: 6,
  });

  return (
    <Card className="w-full">
      <CardHeader>
        <CardTitle>{t("admin:reconciliation.title")}</CardTitle>
        <CardDescription>
          {t("admin:reconciliation.description")}
        </CardDescription>
      </CardHeader>
      <CardContent>
        {runs.isPending && <Skeleton className="h-40 w-full" />}
        {!runs.isPending && (runs.data ?? []).length === 0 && (
          <p className="text-muted-foreground text-sm">
            {t("admin:reconciliation.empty")}
          </p>
        )}
        {(runs.data ?? []).length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("admin:reconciliation.period")}</TableHead>
                <TableHead>{t("admin:reconciliation.status")}</TableHead>
                <TableHead className="text-right">
                  {t("admin:reconciliation.organizations")}
                </TableHead>
                <TableHead className="text-right">
                  {t("admin:reconciliation.reaped")}
                </TableHead>
                <TableHead className="text-right">
                  {t("admin:reconciliation.unattributed")}
                </TableHead>
                <TableHead>
                  <span className="sr-only">
                    {t("admin:reconciliation.details")}
                  </span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(runs.data ?? []).map((run) => {
                const findings = run.findings as Findings;
                const status = run.status as keyof typeof STATUS_VARIANT;
                const unattributed = (findings.unattributed ?? []).reduce(
                  (sum, u) => sum + u.costUsd,
                  0,
                );
                return (
                  <TableRow key={run.id} className="align-top">
                    <TableCell className="whitespace-nowrap">
                      {run.periodStart.slice(0, 10)}
                    </TableCell>
                    <TableCell>
                      <Badge variant={STATUS_VARIANT[status] ?? "outline"}>
                        {status === "ok"
                          ? t("admin:reconciliation.statuses.ok")
                          : status === "mismatch"
                            ? t("admin:reconciliation.statuses.mismatch")
                            : t("admin:reconciliation.statuses.error")}
                      </Badge>
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {findings.organizations?.length ?? 0}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {(findings.reapedHolds ?? []).reduce(
                        (sum, r) => sum + r.holds,
                        0,
                      )}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {usd.format(unattributed)}
                    </TableCell>
                    <TableCell className="text-right">
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() =>
                          setOpenId(openId === run.id ? null : run.id)
                        }
                      >
                        {t("admin:reconciliation.details")}
                      </Button>
                      {openId === run.id && (
                        <pre className="bg-muted mt-2 max-w-xl overflow-x-auto rounded p-3 text-left text-xs">
                          {JSON.stringify(findings, null, 2)}
                        </pre>
                      )}
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
};
