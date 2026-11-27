"use client";

import { useMutation, useQuery } from "@tanstack/react-query";
import { useState } from "react";

import { handle } from "@workspace/api/utils";
import { useTranslation } from "@workspace/i18n";
import { Alert, AlertDescription, AlertTitle } from "@workspace/ui-web/alert";
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
import { Icons } from "@workspace/ui-web/icons";
import { Input } from "@workspace/ui-web/input";
import { Skeleton } from "@workspace/ui-web/skeleton";
import { Spinner } from "@workspace/ui-web/spinner";

import { api } from "~/lib/api/client";
import { formatMicroUsd, MICRO_USD, usdToMicroUsd } from "~/modules/keys/money";

interface TopUpProps {
  readonly organizationId: string;
}

/**
 * Buy credits: the limits, quick picks and fee come from the admin settings
 * (ADMIN-04, ADMIN-05) and the fee is shown before paying. The API checks the
 * same limits. The balance changes when the payment webhook arrives, not here.
 * While staff have top-ups off, or no payment provider can take them
 * (ADMIN-03), a notice replaces the form.
 */
export const TopUp = ({ organizationId }: TopUpProps) => {
  const options = useQuery({
    queryKey: ["credits", organizationId, "top-up", "options"],
    queryFn: () =>
      handle(api.credits[":organizationId"]["top-up"].options.$get)({
        param: { organizationId },
      }),
  });

  if (!options.data) {
    return <Skeleton className="h-64 w-full" />;
  }
  if (!options.data.isAvailable) {
    return <TopUpUnavailable />;
  }
  return <TopUpForm organizationId={organizationId} options={options.data} />;
};

const TopUpUnavailable = () => {
  const { t } = useTranslation(["common", "organization"]);
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("organization:credits.topUp.title")}</CardTitle>
      </CardHeader>
      <CardContent>
        <Alert>
          <Icons.Info />
          <AlertTitle>
            {t("organization:credits.topUp.unavailable.title")}
          </AlertTitle>
          <AlertDescription>
            {t("organization:credits.topUp.unavailable.description")}
          </AlertDescription>
        </Alert>
      </CardContent>
    </Card>
  );
};

interface TopUpOptions {
  minMicroUsd: number;
  maxMicroUsd: number;
  presetsMicroUsd: number[];
}

interface TopUpFormProps {
  readonly organizationId: string;
  readonly options: TopUpOptions;
}

const TopUpForm = ({ organizationId, options }: TopUpFormProps) => {
  const { t, i18n } = useTranslation(["common", "organization"]);
  const presetsUsd = options.presetsMicroUsd.map((preset) =>
    String(preset / MICRO_USD),
  );
  const [amountUsd, setAmountUsd] = useState(
    presetsUsd[Math.min(1, presetsUsd.length - 1)] ?? "",
  );

  const amount = toCreditMicroUsd(amountUsd, options);
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
            {options.presetsMicroUsd.map((pick, index) => (
              <Button
                key={pick}
                type="button"
                variant={
                  amountUsd === presetsUsd[index] ? "default" : "outline"
                }
                onClick={() => setAmountUsd(presetsUsd[index] ?? "")}
                disabled={isRedirecting}
              >
                {money(pick)}
              </Button>
            ))}
          </div>
        </div>
        {amount === null && (
          <FieldError>
            {t("organization:credits.topUp.invalid", {
              min: money(options.minMicroUsd),
              max: money(options.maxMicroUsd),
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
const toCreditMicroUsd = (
  value: string,
  { minMicroUsd, maxMicroUsd }: TopUpOptions,
) => {
  const trimmed = value.trim();
  if (!/^\d+(\.\d{1,2})?$/.test(trimmed)) {
    return null;
  }
  const microUsd = usdToMicroUsd(Number(trimmed));
  return microUsd >= minMicroUsd && microUsd <= maxMicroUsd ? microUsd : null;
};
