"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { handle } from "@workspace/api/utils";
import { useTranslation } from "@workspace/i18n";
import { formatRateE6 } from "@workspace/shared/utils";
import { Badge } from "@workspace/ui-web/badge";
import { Button } from "@workspace/ui-web/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui-web/card";
import { Input } from "@workspace/ui-web/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@workspace/ui-web/select";
import { Skeleton } from "@workspace/ui-web/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@workspace/ui-web/table";
import {
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "@workspace/ui-web/tabs";
import { toast } from "@workspace/ui-web/toast";

import { api } from "~/lib/api/client";
import { useStaffPermission } from "~/modules/admin/lib/use-staff-permission";
import { isTokenField, perMillion } from "~/modules/admin/model-prices/format";
import { GatewayCheck } from "~/modules/admin/model-prices/gateway-check";
import { HistorySheet } from "~/modules/admin/model-prices/history-sheet";
import { PriceForm } from "~/modules/admin/model-prices/price-form";
import { SuggestionsInbox } from "~/modules/admin/model-prices/suggestions-inbox";

import type { PriceFormStart } from "~/modules/admin/model-prices/price-form";

const SCOPES = ["all", "provider", "key"] as const;
type Scope = (typeof SCOPES)[number];

const NO_RATES: PriceFormStart["rates"] = {
  inputUsd: null,
  outputUsd: null,
  cacheReadUsd: null,
  cacheWriteUsd: null,
  cacheWrite1hUsd: null,
  otherRates: {},
};

/** The rates the list shows first; the rest are counted. */
const SHOWN = {
  input_cost_per_token: "admin:modelPrices.field.input_cost_per_token",
  output_cost_per_token: "admin:modelPrices.field.output_cost_per_token",
  cache_read_input_token_cost:
    "admin:modelPrices.field.cache_read_input_token_cost",
  cache_creation_input_token_cost:
    "admin:modelPrices.field.cache_creation_input_token_cost",
} as const;
const isShown = (field: string): field is keyof typeof SHOWN => field in SHOWN;

/**
 * Our model price table (ADMIN-09, ADR 0018): what each model costs now at
 * each provider, and at keys with a negotiated price, in USD and in SAR at the
 * effective rate; its history, a new version, the datasheet's suggestions and
 * whether the gateway charges exactly this.
 */
export const ModelPrices = () => {
  const { t, i18n } = useTranslation(["common", "admin"]);
  const queryClient = useQueryClient();
  const canEdit = useStaffPermission({ pricing: ["update"] });
  const [search, setSearch] = useState("");
  const [provider, setProvider] = useState("");
  const [scope, setScope] = useState<Scope>("all");
  const [history, setHistory] = useState<{
    provider: string;
    model: string;
  } | null>(null);
  const [form, setForm] = useState<PriceFormStart | null>(null);

  const query = {
    ...(search.trim() ? { q: search.trim() } : {}),
    ...(provider.trim() ? { provider: provider.trim() } : {}),
    ...(scope === "all" ? {} : { scope }),
  };
  const list = useQuery({
    queryKey: ["admin", "model-prices", "list", query],
    queryFn: () => handle(api.admin["model-prices"].$get)({ query }),
  });
  const imported = useMutation({
    mutationFn: () => handle(api.admin["model-prices"].import.$post)(),
    onSuccess: async (result) => {
      await queryClient.invalidateQueries({
        queryKey: ["admin", "model-prices"],
      });
      toast.add({
        title: t("admin:modelPrices.import.done", {
          imported: result.imported,
          existing: result.existing,
        }),
        type: "success",
      });
    },
    onError: (error) => toast.add({ title: error.message, type: "error" }),
  });

  const pushedBy = new Map(
    (list.data?.pushStates ?? []).map((state) => [
      JSON.stringify([state.provider, state.model, state.providerKeyId]),
      state,
    ]),
  );
  const scopes = SCOPES.map((value) => ({
    value,
    label: t(`admin:modelPrices.list.scope.${value}`),
  }));
  const rateE6 = list.data?.effectiveUsdToSarRateE6;

  return (
    <div className="flex w-full flex-col gap-6">
      <Card>
        <CardHeader className="flex flex-wrap items-start justify-between gap-4">
          <div className="flex flex-col gap-1.5">
            <CardTitle>{t("admin:modelPrices.header.title")}</CardTitle>
            <CardDescription>
              {t("admin:modelPrices.header.description")}
              {rateE6 !== undefined &&
                ` ${t("admin:modelPrices.header.rate", { rate: formatRateE6(rateE6) })}`}
            </CardDescription>
          </div>
          {canEdit && (
            <div className="flex gap-2">
              <Button
                variant="outline"
                disabled={imported.isPending}
                onClick={() => imported.mutate()}
              >
                {t("admin:modelPrices.import.button")}
              </Button>
              <Button
                onClick={() =>
                  setForm({
                    provider: "",
                    model: "",
                    providerKeyId: null,
                    rates: NO_RATES,
                    isExisting: false,
                  })
                }
              >
                {t("admin:modelPrices.list.new")}
              </Button>
            </div>
          )}
        </CardHeader>
        <CardContent>
          <Tabs defaultValue="prices">
            <TabsList>
              <TabsTrigger value="prices">
                {t("admin:modelPrices.tabs.prices")}
              </TabsTrigger>
              <TabsTrigger value="suggestions">
                {t("admin:modelPrices.tabs.suggestions")}
              </TabsTrigger>
              <TabsTrigger value="check">
                {t("admin:modelPrices.tabs.check")}
              </TabsTrigger>
            </TabsList>

            <TabsContent value="prices" className="mt-4 flex flex-col gap-4">
              <div className="flex flex-wrap gap-2">
                <Input
                  className="w-64"
                  placeholder={t("admin:modelPrices.list.search")}
                  aria-label={t("admin:modelPrices.list.search")}
                  value={search}
                  onChange={(event) => setSearch(event.target.value)}
                />
                <Input
                  className="w-48"
                  placeholder={t("admin:modelPrices.list.provider")}
                  aria-label={t("admin:modelPrices.list.provider")}
                  value={provider}
                  onChange={(event) => setProvider(event.target.value)}
                />
                <Select
                  items={scopes}
                  value={scope}
                  onValueChange={(value) =>
                    setScope(SCOPES.find((item) => item === value) ?? "all")
                  }
                >
                  <SelectTrigger
                    className="w-56"
                    aria-label={t("admin:modelPrices.list.key")}
                  >
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {scopes.map((item) => (
                      <SelectItem key={item.value} value={item.value}>
                        {item.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>

              {list.isPending && <Skeleton className="h-48 w-full" />}
              {list.data?.prices.length === 0 && (
                <p className="text-muted-foreground text-sm">
                  {t("admin:modelPrices.list.empty")}
                </p>
              )}
              {list.data && list.data.prices.length > 0 && (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>{t("admin:modelPrices.list.model")}</TableHead>
                      <TableHead>{t("admin:modelPrices.list.key")}</TableHead>
                      <TableHead>{t("admin:modelPrices.list.rates")}</TableHead>
                      <TableHead>{t("admin:modelPrices.list.since")}</TableHead>
                      <TableHead>
                        {t("admin:modelPrices.list.gateway")}
                      </TableHead>
                      <TableHead />
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {list.data.prices.map((price) => {
                      const state = pushedBy.get(
                        JSON.stringify([
                          price.provider,
                          price.model,
                          price.providerKeyId,
                        ]),
                      );
                      const isOnGateway =
                        state?.pushedPriceId === price.id && !state.lastError;
                      const shown = price.rates.flatMap(({ field, usd, sar }) =>
                        isShown(field) ? [{ field, usd, sar }] : [],
                      );
                      const more = price.rates.length - shown.length;
                      return (
                        <TableRow key={price.id}>
                          <TableCell className="font-mono text-xs">
                            {price.provider}/{price.model}
                          </TableCell>
                          <TableCell className="font-mono text-xs">
                            {price.providerKeyId ??
                              t("admin:modelPrices.list.allKeys")}
                          </TableCell>
                          <TableCell className="text-xs tabular-nums">
                            {shown.map((rate) => (
                              <div key={rate.field}>
                                {t(SHOWN[rate.field])}:{" "}
                                {isTokenField(rate.field)
                                  ? `$${perMillion(rate.usd)} · SAR ${perMillion(rate.sar)} / 1M`
                                  : `$${rate.usd} · SAR ${rate.sar}`}
                              </div>
                            ))}
                            {more > 0 && (
                              <div className="text-muted-foreground">
                                {t("admin:modelPrices.list.moreRates", {
                                  count: more,
                                })}
                              </div>
                            )}
                          </TableCell>
                          <TableCell className="text-xs whitespace-nowrap">
                            {new Date(price.effectiveFrom).toLocaleString(
                              i18n.language,
                            )}
                            {price.scheduledFrom && (
                              <Badge variant="outline" className="ml-2">
                                {t("admin:modelPrices.list.scheduled")}
                              </Badge>
                            )}
                          </TableCell>
                          <TableCell>
                            <Badge
                              variant={
                                isOnGateway ? "secondary" : "destructive"
                              }
                            >
                              {t(
                                isOnGateway
                                  ? "admin:modelPrices.list.pushed"
                                  : "admin:modelPrices.list.notPushed",
                              )}
                            </Badge>
                          </TableCell>
                          <TableCell className="whitespace-nowrap">
                            <Button
                              size="sm"
                              variant="ghost"
                              onClick={() =>
                                setHistory({
                                  provider: price.provider,
                                  model: price.model,
                                })
                              }
                            >
                              {t("admin:modelPrices.list.history")}
                            </Button>
                            {canEdit && (
                              <Button
                                size="sm"
                                variant="outline"
                                onClick={() =>
                                  setForm({
                                    provider: price.provider,
                                    model: price.model,
                                    providerKeyId: price.providerKeyId,
                                    rates: {
                                      inputUsd: price.inputUsd,
                                      outputUsd: price.outputUsd,
                                      cacheReadUsd: price.cacheReadUsd,
                                      cacheWriteUsd: price.cacheWriteUsd,
                                      cacheWrite1hUsd: price.cacheWrite1hUsd,
                                      otherRates: price.otherRates,
                                    },
                                    isExisting: true,
                                  })
                                }
                              >
                                {t("admin:modelPrices.list.edit")}
                              </Button>
                            )}
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              )}
            </TabsContent>

            <TabsContent value="suggestions" className="mt-4">
              <SuggestionsInbox canEdit={canEdit} />
            </TabsContent>

            <TabsContent value="check" className="mt-4">
              <GatewayCheck canEdit={canEdit} />
            </TabsContent>
          </Tabs>
        </CardContent>
      </Card>

      <HistorySheet target={history} onClose={() => setHistory(null)} />
      <PriceForm start={form} onClose={() => setForm(null)} />
    </div>
  );
};
