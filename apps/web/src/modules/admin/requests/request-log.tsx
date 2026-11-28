"use client";

import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { parseAsInteger, parseAsString, useQueryStates } from "nuqs";

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

import { pathsConfig } from "~/config/paths";
import { admin } from "~/modules/admin/lib/api";
import { TurboLink } from "~/modules/common/turbo-link";

const PAGE_SIZE = 50;
const STATUSES = ["all", "success", "error"] as const;

/**
 * Every request the gateway logged, newest first: what happened to it, never
 * what was said. Support filters by outcome, or by organization from the link
 * in a row.
 */
export const RequestLog = () => {
  const { t, i18n } = useTranslation(["common", "admin"]);
  const [filters, setFilters] = useQueryStates({
    status: parseAsString.withDefault("all"),
    organization: parseAsString,
    page: parseAsInteger.withDefault(0),
  });

  const log = useQuery({
    ...admin.queries.requests.list({
      limit: String(PAGE_SIZE),
      offset: String(filters.page * PAGE_SIZE),
      ...(filters.status === "success" || filters.status === "error"
        ? { status: filters.status }
        : {}),
      ...(filters.organization ? { organizationId: filters.organization } : {}),
    }),
    placeholderData: keepPreviousData,
  });
  const rows = log.data?.requests ?? [];
  const total = log.data?.total ?? 0;
  const isLastPage = (filters.page + 1) * PAGE_SIZE >= total;

  return (
    <Card className="w-full">
      <CardHeader>
        <CardTitle>{t("admin:requests.header.title")}</CardTitle>
        <CardDescription>
          {t("admin:requests.header.description")}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex flex-wrap items-center gap-2">
          {STATUSES.map((status) => (
            <Button
              key={status}
              type="button"
              size="sm"
              variant={filters.status === status ? "default" : "outline"}
              onClick={() => setFilters({ status, page: 0 })}
            >
              {t(`admin:requests.status.${status}`)}
            </Button>
          ))}
          {filters.organization && (
            <Button
              type="button"
              size="sm"
              variant="ghost"
              onClick={() => setFilters({ organization: null, page: 0 })}
            >
              {t("admin:requests.allOrganizations")}
            </Button>
          )}
          <span className="text-muted-foreground ml-auto text-sm">
            {t("admin:requests.total", { count: total })}
          </span>
        </div>

        {log.isError && (
          <p className="text-destructive text-sm">
            {t("admin:requests.unavailable")}
          </p>
        )}
        {log.isPending && <Skeleton className="h-40 w-full" />}
        {log.data && rows.length === 0 && (
          <p className="text-muted-foreground text-sm">
            {t("admin:requests.empty")}
          </p>
        )}
        {rows.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("admin:requests.columns.when")}</TableHead>
                <TableHead>
                  {t("admin:requests.columns.organization")}
                </TableHead>
                <TableHead>{t("admin:requests.columns.model")}</TableHead>
                <TableHead>{t("admin:requests.columns.outcome")}</TableHead>
                <TableHead className="text-right">
                  {t("admin:requests.columns.latency")}
                </TableHead>
                <TableHead className="text-right">
                  {t("admin:requests.columns.tokens")}
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((request) => (
                <TableRow key={request.id} data-request={request.id}>
                  <TableCell className="text-muted-foreground whitespace-nowrap">
                    {new Date(request.timestamp).toLocaleString(i18n.language)}
                  </TableCell>
                  <TableCell>
                    {request.organization ? (
                      <span className="flex flex-wrap items-center gap-2">
                        <TurboLink
                          className="underline underline-offset-4"
                          href={pathsConfig.admin.organizations.organization(
                            request.organization.id,
                          )}
                        >
                          {request.organization.name}
                        </TurboLink>
                        <Button
                          type="button"
                          size="sm"
                          variant="ghost"
                          className="h-6 px-2 text-xs"
                          onClick={() =>
                            setFilters({
                              organization: request.organization?.id ?? null,
                              page: 0,
                            })
                          }
                        >
                          {t("admin:requests.onlyThis")}
                        </Button>
                      </span>
                    ) : (
                      <span className="text-muted-foreground">
                        {t("admin:requests.noOrganization")}
                      </span>
                    )}
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {request.provider}/{request.model}
                  </TableCell>
                  <TableCell>
                    <div className="flex flex-col gap-1">
                      <Badge
                        className="w-fit"
                        variant={
                          request.status === "error"
                            ? "destructive"
                            : "secondary"
                        }
                      >
                        {request.status}
                      </Badge>
                      {request.error && (
                        <span className="text-destructive max-w-80 text-xs break-words">
                          {request.error}
                        </span>
                      )}
                    </div>
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {request.latencyMs === null
                      ? ""
                      : `${Math.round(request.latencyMs)} ms`}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {request.promptTokens ?? 0} /{" "}
                    {request.completionTokens ?? 0}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}

        <div className="flex justify-end gap-2">
          <Button
            type="button"
            size="sm"
            variant="outline"
            disabled={filters.page === 0}
            onClick={() => setFilters({ page: filters.page - 1 })}
          >
            {t("admin:requests.newer")}
          </Button>
          <Button
            type="button"
            size="sm"
            variant="outline"
            disabled={isLastPage}
            onClick={() => setFilters({ page: filters.page + 1 })}
          >
            {t("admin:requests.older")}
          </Button>
        </div>
      </CardContent>
    </Card>
  );
};
