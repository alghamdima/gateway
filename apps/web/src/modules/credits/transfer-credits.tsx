"use client";

import { standardSchemaResolver } from "@hookform/resolvers/standard-schema";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Controller, useForm, useWatch } from "react-hook-form";
import * as z from "zod";

import { handle } from "@workspace/api/utils";
import { useTranslation } from "@workspace/i18n";
import { Button, buttonVariants } from "@workspace/ui-web/button";
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@workspace/ui-web/select";
import { Skeleton } from "@workspace/ui-web/skeleton";
import { Spinner } from "@workspace/ui-web/spinner";
import { toast } from "@workspace/ui-web/toast";

import { pathsConfig } from "~/config/paths";
import { api } from "~/lib/api/client";
import { TurboLink } from "~/modules/common/turbo-link";
import { formatMicroSar } from "~/modules/keys/money";

const MICRO_SAR = 1_000_000;
const MICRO_SAR_PER_HALALA = 10_000;

/** Riyals as typed, with at most two decimals, to micro-SAR; null when it is not an amount. */
const toMicroSar = (text: string) =>
  /^\d+(\.\d{1,2})?$/.test(text.trim())
    ? Math.round(Number(text.trim()) * 100) * MICRO_SAR_PER_HALALA
    : null;

const newKey = () => crypto.randomUUID();

interface TransferCreditsProps {
  /** The account whose credits page this is. The card shows only in the user's personal account. */
  readonly organizationId: string;
}

/**
 * Move credits to an organization (BILL-12, ADR 0020): from the user's
 * personal account to an organization they belong to. One way; bought credit
 * only; a fresh two-factor code at every move.
 */
export const TransferCredits = ({ organizationId }: TransferCreditsProps) => {
  const { t, i18n } = useTranslation(["common", "organization"]);
  const queryClient = useQueryClient();
  // one key per move: a double click or a retry sends the same one and moves nothing twice
  const [idempotencyKey, setIdempotencyKey] = useState(newKey);

  const options = useQuery({
    queryKey: ["credits", organizationId, "transfer-options"],
    queryFn: () => handle(api.credits.transfer.options.$get)(),
  });
  const money = (microSar: number) => formatMicroSar(microSar, i18n.language);
  const transferable = options.data?.transferableMicroSar ?? 0;

  const schema = z.object({
    toOrganizationId: z
      .string()
      .min(1, t("organization:credits.transfer.to.required")),
    amount: z.string().refine(
      (text) => {
        const amount = toMicroSar(text);
        return amount !== null && amount > 0 && amount <= transferable;
      },
      {
        message: t("organization:credits.transfer.amount.invalid", {
          max: money(transferable),
        }),
      },
    ),
    code: z
      .string()
      .trim()
      .regex(/^\d{6}$/, t("organization:credits.transfer.code.invalid")),
  });
  const form = useForm({
    resolver: standardSchemaResolver(schema),
    defaultValues: { toOrganizationId: "", amount: "", code: "" },
  });

  const chosenId = useWatch({
    control: form.control,
    name: "toOrganizationId",
  });

  const transfer = useMutation({
    mutationFn: (values: z.infer<typeof schema>) =>
      handle(api.credits.transfer.$post)({
        json: {
          toOrganizationId: values.toOrganizationId,
          amountMicroSar: toMicroSar(values.amount) ?? 0,
          code: values.code.trim(),
          idempotencyKey,
        },
      }),
    onSuccess: async (result) => {
      setIdempotencyKey(newKey());
      form.reset();
      await queryClient.invalidateQueries({ queryKey: ["credits"] });
      toast.add({
        title: t("organization:credits.transfer.done", {
          amount: money(result.amountMicroSar),
          organization:
            options.data?.destinations.find(
              (destination) => destination.id === result.toOrganizationId,
            )?.name ?? "",
        }),
        type: "success",
      });
    },
    // Say why (this replaces the default handler, which would have). A wrong
    // code is typed again; the key stays, so the same move is not sent twice.
    onError: (error: Error) => {
      toast.add({ title: error.message, type: "error" });
      form.resetField("code");
    },
  });

  // only in the personal account: credit never leaves an organization
  if (options.data?.personalOrganizationId !== organizationId) {
    return null;
  }

  const destinations = options.data.destinations.map((destination) => ({
    value: destination.id,
    label: destination.name,
    ...destination,
  }));
  const chosen = destinations.find(
    (destination) => destination.id === chosenId,
  );

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("organization:credits.transfer.title")}</CardTitle>
        <CardDescription>
          {t("organization:credits.transfer.description")}
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        {options.isPending && <Skeleton className="h-24 w-full" />}
        {!options.data.isTwoFactorEnabled && (
          <p className="text-sm">
            {t("organization:credits.transfer.twoFactor.required")}{" "}
            <TurboLink
              href={pathsConfig.dashboard.user.settings.security}
              className={buttonVariants({ variant: "link", size: "sm" })}
            >
              {t("organization:credits.transfer.twoFactor.cta")}
            </TurboLink>
          </p>
        )}
        {options.data.isTwoFactorEnabled && destinations.length === 0 && (
          <p className="text-muted-foreground text-sm">
            {t("organization:credits.transfer.noOrganizations")}
          </p>
        )}
        {options.data.isTwoFactorEnabled && destinations.length > 0 && (
          <form
            id="transfer-credits-form"
            noValidate
            onSubmit={form.handleSubmit((values) =>
              transfer.mutateAsync(values).catch(() => undefined),
            )}
            className="flex flex-col gap-4"
          >
            <p className="text-sm">
              {t("organization:credits.transfer.available", {
                amount: money(transferable),
              })}
            </p>
            <div className="flex flex-wrap items-start gap-4">
              <Controller
                name="toOrganizationId"
                control={form.control}
                render={({ field, fieldState }) => (
                  <Field data-invalid={fieldState.invalid} className="w-64">
                    <FieldLabel htmlFor="transfer-credits-to">
                      {t("organization:credits.transfer.to.label")}
                    </FieldLabel>
                    <Select
                      items={destinations}
                      onValueChange={field.onChange}
                      value={field.value}
                      disabled={form.formState.isSubmitting}
                    >
                      <SelectTrigger
                        id="transfer-credits-to"
                        className="w-full"
                        aria-invalid={fieldState.invalid}
                      >
                        <SelectValue
                          placeholder={t(
                            "organization:credits.transfer.to.placeholder",
                          )}
                        />
                      </SelectTrigger>
                      <SelectContent>
                        {destinations.map((destination) => (
                          <SelectItem
                            key={destination.value}
                            value={destination.value}
                          >
                            {destination.label}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    {fieldState.invalid && (
                      <FieldError errors={[fieldState.error]} />
                    )}
                  </Field>
                )}
              />
              <Controller
                name="amount"
                control={form.control}
                render={({ field, fieldState }) => (
                  <Field data-invalid={fieldState.invalid} className="w-48">
                    <FieldLabel htmlFor="transfer-credits-amount">
                      {t("organization:credits.transfer.amount.label")}
                    </FieldLabel>
                    <Input
                      id="transfer-credits-amount"
                      inputMode="decimal"
                      placeholder={(transferable / MICRO_SAR).toFixed(2)}
                      {...field}
                      aria-invalid={fieldState.invalid}
                      disabled={form.formState.isSubmitting}
                    />
                    {fieldState.invalid && (
                      <FieldError errors={[fieldState.error]} />
                    )}
                  </Field>
                )}
              />
              <Controller
                name="code"
                control={form.control}
                render={({ field, fieldState }) => (
                  <Field data-invalid={fieldState.invalid} className="w-48">
                    <FieldLabel htmlFor="transfer-credits-code">
                      {t("organization:credits.transfer.code.label")}
                    </FieldLabel>
                    <Input
                      id="transfer-credits-code"
                      inputMode="numeric"
                      autoComplete="one-time-code"
                      maxLength={6}
                      {...field}
                      aria-invalid={fieldState.invalid}
                      disabled={form.formState.isSubmitting}
                    />
                    {fieldState.invalid && (
                      <FieldError errors={[fieldState.error]} />
                    )}
                  </Field>
                )}
              />
              <Button
                type="submit"
                className="mt-6"
                disabled={
                  form.formState.isSubmitting ||
                  transferable === 0 ||
                  !!chosen?.waitingFor
                }
              >
                {form.formState.isSubmitting ? (
                  <Spinner />
                ) : (
                  t("organization:credits.transfer.submit")
                )}
              </Button>
            </div>
            {chosen?.waitingFor && chosen.availableAt && (
              <p className="text-sm" role="status">
                {t(
                  `organization:credits.transfer.waiting.${chosen.waitingFor}`,
                  {
                    when: new Date(chosen.availableAt).toLocaleString(
                      i18n.language,
                    ),
                  },
                )}
              </p>
            )}
            <FieldDescription>
              {t("organization:credits.transfer.rules")}
            </FieldDescription>
          </form>
        )}
      </CardContent>
    </Card>
  );
};
