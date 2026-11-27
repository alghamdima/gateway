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
import { formatMicroSar, MICRO_SAR, sarToMicroSar } from "~/modules/keys/money";

import type { InferResponseType } from "hono/client";

type TopUpQuote = InferResponseType<
  (typeof api.credits)[":organizationId"]["top-up"]["quote"]["$get"],
  200
>;
/** A reason the server gives for refusing a code (billing:promo.refused). */
type PromoRefusal = Extract<
  NonNullable<TopUpQuote["promo"]>,
  { status: "refused" }
>["reason"];

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
  minMicroSar: number;
  maxMicroSar: number;
  presetsMicroSar: number[];
}

interface TopUpFormProps {
  readonly organizationId: string;
  readonly options: TopUpOptions;
}

const TopUpForm = ({ organizationId, options }: TopUpFormProps) => {
  const { t, i18n } = useTranslation(["common", "organization"]);
  const presetsUsd = options.presetsMicroSar.map((preset) =>
    String(preset / MICRO_SAR),
  );
  const [amountSar, setAmountUsd] = useState(
    presetsUsd[Math.min(1, presetsUsd.length - 1)] ?? "",
  );

  // what is typed, and the code applied to the quote (BILL-08)
  const [codeInput, setCodeInput] = useState("");
  const [promoCode, setPromoCode] = useState<string | null>(null);

  const amount = toCreditMicroSar(amountSar, options);
  const money = (microSar: number) => formatMicroSar(microSar, i18n.language);

  const quote = useQuery({
    queryKey: ["credits", organizationId, "top-up", amount, promoCode],
    queryFn: () =>
      handle(api.credits[":organizationId"]["top-up"].quote.$get)({
        param: { organizationId },
        query: {
          amount: String(amount),
          ...(promoCode ? { code: promoCode } : {}),
        },
      }),
    enabled: amount !== null,
  });
  const promo = quote.data?.promo ?? null;
  // only a code the server applied goes to the checkout, which checks it again
  const appliedCode = promo?.status === "applied" ? promo.code : undefined;

  const checkout = useMutation({
    mutationFn: (creditMicroSar: number) =>
      handle(api.credits[":organizationId"]["top-up"].$post)({
        param: { organizationId },
        json: {
          amount: creditMicroSar,
          ...(appliedCode ? { promoCode: appliedCode } : {}),
        },
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
          value: quote.data.creditMicroSar,
        },
        {
          label: t("organization:credits.topUp.fee"),
          value: quote.data.baseFeeMicroSar,
        },
        ...(appliedCode && quote.data.feeDiscountMicroSar > 0
          ? [
              {
                label: t("organization:credits.topUp.promo.discount", {
                  code: appliedCode,
                }),
                value: -quote.data.feeDiscountMicroSar,
              },
            ]
          : []),
        {
          label: t("organization:credits.topUp.total"),
          value: quote.data.totalMicroSar,
        },
      ]
    : [];
  const received =
    quote.data && appliedCode && quote.data.bonusMicroSar > 0
      ? [
          {
            label: t("organization:credits.topUp.promo.bonus", {
              code: appliedCode,
            }),
            value: quote.data.bonusMicroSar,
          },
          {
            label: t("organization:credits.topUp.promo.received"),
            value: quote.data.creditReceivedMicroSar,
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
              <span className="text-muted-foreground">SAR</span>
              <Input
                id="top-up-amount"
                inputMode="decimal"
                value={amountSar}
                onChange={(event) => setAmountUsd(event.target.value)}
                aria-invalid={amount === null}
                disabled={isRedirecting}
              />
            </div>
          </Field>
          <div className="flex flex-wrap gap-2">
            {options.presetsMicroSar.map((pick, index) => (
              <Button
                key={pick}
                type="button"
                variant={
                  amountSar === presetsUsd[index] ? "default" : "outline"
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
              min: money(options.minMicroSar),
              max: money(options.maxMicroSar),
            })}
          </FieldError>
        )}

        <PromoCodeField
          value={codeInput}
          onChange={setCodeInput}
          appliedCode={appliedCode}
          refusal={promo?.status === "refused" ? promo.reason : null}
          isBusy={isRedirecting || (promoCode !== null && quote.isFetching)}
          onApply={() => setPromoCode(codeInput.trim() || null)}
          onRemove={() => {
            setPromoCode(null);
            setCodeInput("");
          }}
        />

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
            {received.map((line, index) => (
              <div key={line.label} className="contents">
                <dt
                  className={
                    index === received.length - 1
                      ? "pt-2 font-semibold"
                      : "text-muted-foreground pt-2"
                  }
                >
                  {line.label}
                </dt>
                <dd
                  className={`pt-2 text-right tabular-nums ${index === received.length - 1 ? "font-semibold" : "text-green-600"}`}
                >
                  {index === 0 ? "+" : ""}
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
                total: quote.data ? money(quote.data.totalMicroSar) : "",
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

interface PromoCodeFieldProps {
  readonly value: string;
  readonly onChange: (value: string) => void;
  /** The code as the server applied it, normalized. */
  readonly appliedCode: string | undefined;
  /** Why the server refused the code. */
  readonly refusal: PromoRefusal | null;
  readonly isBusy: boolean;
  readonly onApply: () => void;
  readonly onRemove: () => void;
}

/** An optional promo code (BILL-08): applied to the quote, so its effect shows before paying. */
const PromoCodeField = ({
  value,
  onChange,
  appliedCode,
  refusal,
  isBusy,
  onApply,
  onRemove,
}: PromoCodeFieldProps) => {
  const { t } = useTranslation(["organization", "billing"]);
  return (
    <Field data-invalid={refusal !== null} className="max-w-sm">
      <FieldLabel htmlFor="top-up-promo-code">
        {t("organization:credits.topUp.promo.label")}
      </FieldLabel>
      <div className="flex items-center gap-2">
        <Input
          id="top-up-promo-code"
          autoComplete="off"
          placeholder={t("organization:credits.topUp.promo.placeholder")}
          value={appliedCode ?? value}
          onChange={(event) => onChange(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter" && !appliedCode) {
              event.preventDefault();
              onApply();
            }
          }}
          aria-invalid={refusal !== null}
          disabled={isBusy || appliedCode !== undefined}
          className="uppercase"
        />
        {appliedCode ? (
          <Button
            type="button"
            variant="outline"
            onClick={onRemove}
            disabled={isBusy}
          >
            {t("organization:credits.topUp.promo.remove")}
          </Button>
        ) : (
          <Button
            type="button"
            variant="outline"
            onClick={onApply}
            disabled={isBusy || value.trim() === ""}
          >
            {isBusy ? <Spinner /> : t("organization:credits.topUp.promo.apply")}
          </Button>
        )}
      </div>
      {appliedCode && (
        <FieldDescription>
          {t("organization:credits.topUp.promo.applied", { code: appliedCode })}
        </FieldDescription>
      )}
      {refusal && (
        <FieldError>{t(`billing:promo.refused.${refusal}`)}</FieldError>
      )}
    </Field>
  );
};

/** Whole cents within the limits, in micro-SAR; null when the typed amount is not one. */
const toCreditMicroSar = (
  value: string,
  { minMicroSar, maxMicroSar }: TopUpOptions,
) => {
  const trimmed = value.trim();
  if (!/^\d+(\.\d{1,2})?$/.test(trimmed)) {
    return null;
  }
  const microSar = sarToMicroSar(Number(trimmed));
  return microSar >= minMicroSar && microSar <= maxMicroSar ? microSar : null;
};
