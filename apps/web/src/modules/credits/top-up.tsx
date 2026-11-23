"use client";

import { useMutation, useQuery } from "@tanstack/react-query";
import { useState } from "react";

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
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
} from "@workspace/ui-web/field";
import { Input } from "@workspace/ui-web/input";
import { Skeleton } from "@workspace/ui-web/skeleton";
import { Spinner } from "@workspace/ui-web/spinner";

import { api } from "~/lib/api/client";
import { formatMicroUsd, usdToMicroUsd } from "~/modules/keys/money";

/** The API enforces the same limits (docs/platform/adr/0012-top-up-checkout.md). */
const MIN_USD = 5;
const MAX_USD = 1_000;
const QUICK_PICKS = [10, 25, 50, 100];

interface TopUpProps {
  readonly organizationId: string;
}

/**
 * Buy credits: the fee comes from the admin settings and is shown before
 * paying. The balance changes when the payment webhook arrives, not here.
 */
export const TopUp = ({ organizationId }: TopUpProps) => {
  const { t, i18n } = useTranslation(["common", "organization"]);
  const [amountUsd, setAmountUsd] = useState("25");

  const amount = toCreditMicroUsd(amountUsd);
  const money = (microUsd: number) => formatMicroUsd(microUsd, i18n.language);

  const quote = useQuery({
    queryKey: ["credits", organizationId, "top-up", amount],
    queryFn: () =>
      handle(api.credits[":organizationId"]["top-up"].quote.$get)({
        param: { organizationId },
        query: { amount: String(amount) },
      }),
    enabled: amount !== null,
  });

  const checkout = useMutation({
    mutationFn: (creditMicroUsd: number) =>
      handle(api.credits[":organizationId"]["top-up"].$post)({
        param: { organizationId },
        json: { amount: creditMicroUsd },
      }),
    onSuccess: ({ url }) => {
      if (url) {
        window.location.assign(url);
      }
    },
  });

  const isRedirecting = checkout.isPending || checkout.isSuccess;
  const lines = quote.data
    ? [
        {
          label: t("organization:credits.topUp.credits"),
          value: quote.data.creditMicroUsd,
        },
        {
          label: t("organization:credits.topUp.fee"),
          value: quote.data.feeMicroUsd,
        },
        {
          label: t("organization:credits.topUp.total"),
          value: quote.data.totalMicroUsd,
        },
      ]
    : [];

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("organization:credits.topUp.title")}</CardTitle>
        <CardDescription>
          {t("organization:credits.topUp.description")}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-6">
        <div className="flex flex-wrap items-end gap-4">
          <Field data-invalid={amount === null} className="w-48">
            <FieldLabel htmlFor="top-up-amount">
              {t("organization:credits.topUp.amount")}
            </FieldLabel>
            <div className="flex items-center gap-2">
              <span className="text-muted-foreground">$</span>
              <Input
                id="top-up-amount"
                inputMode="decimal"
                value={amountUsd}
                onChange={(event) => setAmountUsd(event.target.value)}
                aria-invalid={amount === null}
                disabled={isRedirecting}
              />
            </div>
          </Field>
          <div className="flex flex-wrap gap-2">
            {QUICK_PICKS.map((pick) => (
              <Button
                key={pick}
                type="button"
                variant={amountUsd === String(pick) ? "default" : "outline"}
                onClick={() => setAmountUsd(String(pick))}
                disabled={isRedirecting}
              >
                {money(pick * 1_000_000)}
              </Button>
            ))}
          </div>
        </div>
        {amount === null && (
          <FieldError>
            {t("organization:credits.topUp.invalid", {
              min: money(MIN_USD * 1_000_000),
              max: money(MAX_USD * 1_000_000),
            })}
          </FieldError>
        )}

        {amount !== null && quote.isPending && (
          <Skeleton className="h-20 w-64" />
        )}
        {amount !== null && quote.data && (
          <dl className="grid max-w-sm grid-cols-[1fr_auto] gap-x-8 gap-y-1">
            {lines.map((line, index) => (
              <div key={line.label} className="contents">
                <dt
                  className={
                    index === lines.length - 1
                      ? "font-semibold"
                      : "text-muted-foreground"
                  }
                >
                  {line.label}
                </dt>
                <dd
                  className={`text-right tabular-nums ${index === lines.length - 1 ? "font-semibold" : ""}`}
                >
                  {money(line.value)}
                </dd>
              </div>
            ))}
          </dl>
        )}

        <div className="space-y-2">
          <Button
            type="button"
            disabled={amount === null || !quote.data || isRedirecting}
            onClick={() => amount !== null && checkout.mutate(amount)}
          >
            {isRedirecting ? (
              <Spinner />
            ) : (
              t("organization:credits.topUp.pay", {
                total: quote.data ? money(quote.data.totalMicroUsd) : "",
              })
            )}
          </Button>
          <FieldDescription>
            {t("organization:credits.topUp.feeNote")}
          </FieldDescription>
        </div>
      </CardContent>
    </Card>
  );
};

/** Whole cents within the limits, in micro-USD; null when the typed amount is not one. */
const toCreditMicroUsd = (value: string) => {
  const trimmed = value.trim();
  if (!/^\d+(\.\d{1,2})?$/.test(trimmed)) {
    return null;
  }
  const usd = Number(trimmed);
  return usd >= MIN_USD && usd <= MAX_USD ? usdToMicroUsd(usd) : null;
};
