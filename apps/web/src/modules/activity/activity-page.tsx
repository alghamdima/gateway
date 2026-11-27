"use client";

import { useQuery } from "@tanstack/react-query";
import { parseAsInteger, useQueryState } from "nuqs";
import { Bar, BarChart, CartesianGrid, XAxis, YAxis } from "recharts";

import { handle } from "@workspace/api/utils";
import { useTranslation } from "@workspace/i18n";
import { Button } from "@workspace/ui-web/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui-web/card";
import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
} from "@workspace/ui-web/chart";
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
import { RecentRequests } from "~/modules/activity/recent-requests";
import { formatMicroSar, MICRO_SAR } from "~/modules/keys/money";

import type { ChartConfig } from "@workspace/ui-web/chart";

const WINDOWS = [7, 30, 90] as const;

interface ActivityPageProps {
  readonly organizationId: string;
}

/**
 * Spend, requests and tokens by day, model and key (step 9.1), from the
 * organization's ledger. Only charged requests are counted.
 */
export const ActivityPage = ({ organizationId }: ActivityPageProps) => {
  const { t, i18n } = useTranslation(["common", "organization"]);
  const [days, setDays] = useQueryState("days", parseAsInteger.withDefault(30));
  const window = WINDOWS.find((value) => value === days) ?? 30;

  const activity = useQuery({
    queryKey: ["activity", organizationId, window],
    queryFn: () =>
      handle(api.activity[":organizationId"].$get)({
        param: { organizationId },
        query: { days: String(window) },
      }),
  });

  const money = (microSar: number) => formatMicroSar(microSar, i18n.language);
  const count = (value: number) =>
    new Intl.NumberFormat(i18n.language).format(value);
  const dayLabel = new Intl.DateTimeFormat(i18n.language, {
    month: "short",
    day: "numeric",
    timeZone: "UTC",
  });

  const chartConfig = {
    spend: {
      label: t("organization:activity.spend"),
      color: "var(--chart-1)",
    },
  } satisfies ChartConfig;

  const totals = activity.data?.totals;
  const cards = totals
    ? [
        { label: t("organization:activity.spend"), value: money(totals.spend) },
        {
          label: t("organization:activity.requests"),
          value: count(totals.requests),
        },
        {
          label: t("organization:activity.tokens"),
          value: count(totals.promptTokens + totals.completionTokens),
        },
      ]
    : [];

  return (
    <div className="flex w-full flex-col gap-6">
      <div className="flex flex-wrap items-center justify-between gap-4">
        <p className="text-muted-foreground text-sm">
          {t("organization:activity.description")}
        </p>
        <div
          className="flex gap-2"
          role="group"
          aria-label={t("organization:activity.window")}
        >
          {WINDOWS.map((value) => (
            <Button
              key={value}
              variant={value === window ? "default" : "outline"}
              size="sm"
              onClick={() => setDays(value)}
            >
              {t("organization:activity.days", { count: value })}
            </Button>
          ))}
        </div>
      </div>

      {activity.isPending ? (
        <Skeleton className="h-24 w-full" />
      ) : (
        <div className="grid gap-4 sm:grid-cols-3">
          {cards.map((card) => (
            <Card key={card.label}>
              <CardHeader>
                <CardDescription>{card.label}</CardDescription>
                <CardTitle className="text-2xl tabular-nums">
                  {card.value}
                </CardTitle>
              </CardHeader>
            </Card>
          ))}
        </div>
      )}

      <Card>
        <CardHeader>
          <CardTitle>{t("organization:activity.byDay")}</CardTitle>
        </CardHeader>
        <CardContent>
          {activity.isPending ? (
            <Skeleton className="h-64 w-full" />
          ) : (
            <ChartContainer config={chartConfig} className="h-64 w-full">
              <BarChart
                accessibilityLayer
                data={(activity.data?.byDay ?? []).map((day) => ({
                  day: day.day,
                  spend: day.spend / MICRO_SAR,
                }))}
              >
                <CartesianGrid vertical={false} />
                <XAxis
                  dataKey="day"
                  tickLine={false}
                  axisLine={false}
                  minTickGap={24}
                  tickFormatter={(value: string) =>
                    dayLabel.format(new Date(value))
                  }
                />
                <YAxis tickLine={false} axisLine={false} width={56} />
                <ChartTooltip
                  content={
                    <ChartTooltipContent
                      labelFormatter={(value) =>
                        dayLabel.format(new Date(String(value)))
                      }
                      formatter={(value) => money(Number(value) * MICRO_SAR)}
                    />
                  }
                />
                <Bar dataKey="spend" fill="var(--color-spend)" radius={4} />
              </BarChart>
            </ChartContainer>
          )}
        </CardContent>
      </Card>

      <div className="grid gap-6 lg:grid-cols-2">
        <BreakdownCard
          title={t("organization:activity.byModel")}
          firstColumn={t("organization:activity.model")}
          isPending={activity.isPending}
          empty={t("organization:activity.empty")}
          rows={(activity.data?.byModel ?? []).map((row) => ({
            key: `${row.provider}/${row.model}`,
            label: `${row.provider}/${row.model}`,
            requests: count(row.requests),
            tokens: count(row.promptTokens + row.completionTokens),
            spend: money(row.spend),
          }))}
        />
        <BreakdownCard
          title={t("organization:activity.byKey")}
          firstColumn={t("organization:activity.key")}
          isPending={activity.isPending}
          empty={t("organization:activity.empty")}
          rows={(activity.data?.byKey ?? []).map((row) => ({
            key: row.keyId ?? "deleted",
            label: row.name ?? t("organization:activity.deletedKey"),
            // names may repeat; the prefix tells keys apart
            detail: row.prefix ? `${row.prefix}…` : undefined,
            requests: count(row.requests),
            tokens: count(row.promptTokens + row.completionTokens),
            spend: money(row.spend),
          }))}
        />
      </div>

      <RecentRequests organizationId={organizationId} />
    </div>
  );
};

interface BreakdownRow {
  key: string;
  label: string;
  detail?: string;
  requests: string;
  tokens: string;
  spend: string;
}

const BreakdownCard = ({
  title,
  firstColumn,
  rows,
  isPending,
  empty,
}: {
  readonly title: string;
  readonly firstColumn: string;
  readonly rows: BreakdownRow[];
  readonly isPending: boolean;
  readonly empty: string;
}) => {
  const { t } = useTranslation("organization");
  return (
    <Card>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
      </CardHeader>
      <CardContent>
        {isPending && <Skeleton className="h-32 w-full" />}
        {!isPending && rows.length === 0 && (
          <p className="text-muted-foreground text-sm">{empty}</p>
        )}
        {rows.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{firstColumn}</TableHead>
                <TableHead className="text-right">
                  {t("activity.requests")}
                </TableHead>
                <TableHead className="text-right">
                  {t("activity.tokens")}
                </TableHead>
                <TableHead className="text-right">
                  {t("activity.spend")}
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((row) => (
                <TableRow key={row.key}>
                  <TableCell>
                    <div className="font-medium">{row.label}</div>
                    {row.detail && (
                      <div className="text-muted-foreground font-mono text-xs">
                        {row.detail}
                      </div>
                    )}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {row.requests}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {row.tokens}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {row.spend}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
};
