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
 * Spend, requests, tokens and timings by day, model and key (step 9.1, SPEC
 * ACT-01/02/04). Money and tokens come from the ledger; requests (one per
 * client request, however many attempts), refusals and timings from the
 * gateway's request log.
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

  const notAvailable = t("organization:activity.notAvailable");
  const maybeCount = (value: number | null) =>
    value === null ? notAvailable : count(value);
  const ms = (value: number) =>
    t("organization:activity.milliseconds", {
      value: count(Math.round(value)),
    });
  const percent = (value: number | null) =>
    value === null
      ? notAvailable
      : new Intl.NumberFormat(i18n.language, {
          style: "percent",
          maximumFractionDigits: 1,
        }).format(value);

  const totals = activity.data?.totals;
  const timings = activity.data?.timings ?? null;
  const cards = totals
    ? [
        { label: t("organization:activity.spend"), value: money(totals.spend) },
        {
          label: t("organization:activity.requests"),
          value: maybeCount(totals.requests),
          detail:
            totals.failed && totals.failed > 0
              ? t("organization:activity.failedRequests", {
                  count: totals.failed,
                })
              : undefined,
        },
        {
          label: t("organization:activity.refused"),
          value: maybeCount(totals.refused),
        },
        {
          label: t("organization:activity.tokens"),
          value: count(totals.promptTokens + totals.completionTokens),
        },
        {
          label: t("organization:activity.cacheHitRate"),
          value: percent(totals.cacheHitRate),
        },
        {
          label: t("organization:activity.timeToFirstToken"),
          value:
            timings && timings.measured > 0
              ? `${ms(timings.timeToFirstToken.p50)} / ${ms(timings.timeToFirstToken.p90)}`
              : notAvailable,
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
                {card.detail && (
                  <CardDescription>{card.detail}</CardDescription>
                )}
              </CardHeader>
            </Card>
          ))}
        </div>
      )}
      {activity.data && (
        <p className="text-muted-foreground text-xs">
          {activity.data.requestLogAvailable
            ? t("organization:activity.requestLogNote")
            : t("organization:activity.requestLogUnavailable")}
        </p>
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
        <TokenCard
          isPending={activity.isPending}
          totals={totals}
          count={count}
        />
        <PerformanceCard
          isPending={activity.isPending}
          timings={timings}
          ms={ms}
          count={count}
        />
      </div>

      <RefusalsCard
        isPending={activity.isPending}
        refusals={activity.data?.refusals ?? []}
        isAvailable={activity.data?.requestLogAvailable ?? true}
        count={count}
      />

      <div className="grid gap-6 lg:grid-cols-2">
        <BreakdownCard
          title={t("organization:activity.byModel")}
          firstColumn={t("organization:activity.model")}
          isPending={activity.isPending}
          empty={t("organization:activity.empty")}
          rows={(activity.data?.byModel ?? []).map((row) => ({
            key: `${row.provider}/${row.model}`,
            label: `${row.provider}/${row.model}`,
            requests: maybeCount(row.requests),
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
            requests: maybeCount(row.requests),
            tokens: count(row.promptTokens + row.completionTokens),
            spend: money(row.spend),
          }))}
        />
      </div>

      <RecentRequests organizationId={organizationId} />
    </div>
  );
};

interface TokenTotals {
  promptTokens: number;
  completionTokens: number;
  reasoningTokens: number;
  cachedTokens: number;
  cacheWriteTokens: number;
}

/** Prompt (cached and not) and completion (with its reasoning), as OpenRouter's token breakdown. */
const TokenCard = ({
  isPending,
  totals,
  count,
}: {
  readonly isPending: boolean;
  readonly totals: TokenTotals | undefined;
  readonly count: (value: number) => string;
}) => {
  const { t } = useTranslation("organization");
  const rows = totals
    ? [
        {
          label: t("activity.promptTokens"),
          value: count(totals.promptTokens),
          parts: [
            [t("activity.cachedTokens"), count(totals.cachedTokens)],
            [
              t("activity.uncachedTokens"),
              count(Math.max(totals.promptTokens - totals.cachedTokens, 0)),
            ],
            [t("activity.cacheWriteTokens"), count(totals.cacheWriteTokens)],
          ],
        },
        {
          label: t("activity.completionTokens"),
          value: count(totals.completionTokens),
          parts: [
            [t("activity.reasoningTokens"), count(totals.reasoningTokens)],
          ],
        },
      ]
    : [];
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("activity.tokenBreakdown")}</CardTitle>
        <CardDescription>
          {t("activity.tokenBreakdownDescription")}
        </CardDescription>
      </CardHeader>
      <CardContent>
        {isPending ? (
          <Skeleton className="h-24 w-full" />
        ) : (
          <dl className="space-y-3 text-sm">
            {rows.map((row) => (
              <div key={row.label}>
                <div className="flex justify-between gap-4">
                  <dt className="font-medium">{row.label}</dt>
                  <dd className="tabular-nums">{row.value}</dd>
                </div>
                {row.parts.map(([label, value]) => (
                  <div
                    key={label}
                    className="text-muted-foreground flex justify-between gap-4 ps-4"
                  >
                    <dt>{label}</dt>
                    <dd className="tabular-nums">{value}</dd>
                  </div>
                ))}
              </div>
            ))}
          </dl>
        )}
      </CardContent>
    </Card>
  );
};

interface Timings {
  measured: number;
  timeToFirstToken: { avg: number; p50: number; p90: number };
  latency: { avg: number; p50: number; p90: number };
  generationTimeAvg: number;
  throughputAvg: number;
}

const PerformanceCard = ({
  isPending,
  timings,
  ms,
  count,
}: {
  readonly isPending: boolean;
  readonly timings: Timings | null;
  readonly ms: (value: number) => string;
  readonly count: (value: number) => string;
}) => {
  const { t } = useTranslation("organization");
  const hasTimings = timings !== null && timings.measured > 0;
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("activity.performance")}</CardTitle>
        <CardDescription>
          {t("activity.performanceDescription")}
        </CardDescription>
      </CardHeader>
      <CardContent>
        {isPending && <Skeleton className="h-24 w-full" />}
        {!isPending && !hasTimings && (
          <p className="text-muted-foreground text-sm">{t("activity.empty")}</p>
        )}
        {hasTimings && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead />
                <TableHead className="text-right">
                  {t("activity.latencyP50")}
                </TableHead>
                <TableHead className="text-right">
                  {t("activity.latencyP90")}
                </TableHead>
                <TableHead className="text-right">
                  {t("activity.latencyAverage")}
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {[
                {
                  label: t("activity.record.latency"),
                  figures: timings.timeToFirstToken,
                },
                { label: t("activity.totalLatency"), figures: timings.latency },
              ].map(({ label, figures }) => (
                <TableRow key={label}>
                  <TableCell className="font-medium">{label}</TableCell>
                  <TableCell className="text-right tabular-nums">
                    {ms(figures.p50)}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {ms(figures.p90)}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {ms(figures.avg)}
                  </TableCell>
                </TableRow>
              ))}
              <TableRow>
                <TableCell className="font-medium">
                  {t("activity.generationTime")}
                </TableCell>
                <TableCell colSpan={3} className="text-right tabular-nums">
                  {ms(timings.generationTimeAvg)}
                </TableCell>
              </TableRow>
              <TableRow>
                <TableCell className="font-medium">
                  {t("activity.throughput")}
                </TableCell>
                <TableCell colSpan={3} className="text-right tabular-nums">
                  {t("activity.throughputValue", {
                    value: count(Math.round(timings.throughputAvg * 10) / 10),
                  })}
                </TableCell>
              </TableRow>
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
};

const RefusalsCard = ({
  isPending,
  refusals,
  isAvailable,
  count,
}: {
  readonly isPending: boolean;
  readonly refusals: {
    limitSource: string;
    reason: string;
    statusCode: number;
    requests: number;
  }[];
  readonly isAvailable: boolean;
  readonly count: (value: number) => string;
}) => {
  const { t } = useTranslation("organization");
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("activity.refusals")}</CardTitle>
        <CardDescription>{t("activity.refusalsDescription")}</CardDescription>
      </CardHeader>
      <CardContent>
        {isPending && <Skeleton className="h-16 w-full" />}
        {!isPending && isAvailable && refusals.length === 0 && (
          <p className="text-muted-foreground text-sm">
            {t("activity.noRefusals")}
          </p>
        )}
        {!isPending && !isAvailable && (
          <p className="text-muted-foreground text-sm">
            {t("activity.requestLogUnavailable")}
          </p>
        )}
        {refusals.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("activity.reason")}</TableHead>
                <TableHead className="text-right">
                  {t("activity.status")}
                </TableHead>
                <TableHead className="text-right">
                  {t("activity.requests")}
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {refusals.map((refusal) => (
                <TableRow
                  key={`${refusal.limitSource}/${refusal.reason}/${refusal.statusCode}`}
                >
                  <TableCell className="font-mono text-xs">
                    {refusal.limitSource}: {refusal.reason}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {refusal.statusCode}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {count(refusal.requests)}
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
