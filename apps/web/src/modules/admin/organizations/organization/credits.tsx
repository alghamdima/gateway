"use client";

import { standardSchemaResolver } from "@hookform/resolvers/standard-schema";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Controller, useForm } from "react-hook-form";
import * as z from "zod";

import { handle } from "@workspace/api/utils";
import { useTranslation } from "@workspace/i18n";
import { Button } from "@workspace/ui-web/button";
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
} from "@workspace/ui-web/field";
import { Input } from "@workspace/ui-web/input";
import { Skeleton } from "@workspace/ui-web/skeleton";
import { Spinner } from "@workspace/ui-web/spinner";
import { toast } from "@workspace/ui-web/toast";

import { api } from "~/lib/api/client";
import { useStaffPermission } from "~/modules/admin/lib/use-staff-permission";
import { formatMicroSar, sarToMicroSar } from "~/modules/keys/money";

interface OrganizationCreditsProps {
  readonly organizationId: string;
}

/**
 * The organization's balance, and a staff adjustment with a required reason
 * (ADR 0001). The adjustment is a ledger entry and lands in the audit log.
 */
export const OrganizationCredits = ({
  organizationId,
}: OrganizationCreditsProps) => {
  const { t, i18n } = useTranslation(["common", "admin"]);
  const queryClient = useQueryClient();
  const canAdjust = useStaffPermission({ credits: ["adjust"] });
  // one key per adjustment the staff member means to make: a double click or a
  // retry of the same submission is applied once
  const [idempotencyKey, setIdempotencyKey] = useState(() =>
    crypto.randomUUID(),
  );

  const queryKey = ["admin", "organizations", organizationId, "credits"];
  const balance = useQuery({
    queryKey,
    queryFn: () =>
      handle(api.admin.organizations[":id"].credits.$get)({
        param: { id: organizationId },
      }),
  });

  const schema = z.object({
    amountSar: z
      .string()
      .trim()
      .refine(
        (value) => {
          const amount = Number(value);
          return (
            value !== "" &&
            Number.isFinite(amount) &&
            amount !== 0 &&
            Math.abs(amount) <= 1_000_000
          );
        },
        {
          message: t("admin:organizations.organization.credits.amount.invalid"),
        },
      ),
    reason: z.string().trim().min(3).max(500),
  });

  const form = useForm({
    resolver: standardSchemaResolver(schema),
    defaultValues: { amountSar: "", reason: "" },
  });

  const adjust = useMutation({
    mutationFn: (input: z.infer<typeof schema>) =>
      handle(api.admin.organizations[":id"].credits.$post)({
        param: { id: organizationId },
        json: {
          amount: sarToMicroSar(Number(input.amountSar)),
          reason: input.reason,
          idempotencyKey,
        },
      }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey });
      form.reset();
      setIdempotencyKey(crypto.randomUUID());
      toast.add({
        title: t("admin:organizations.organization.credits.success"),
        type: "success",
      });
    },
  });

  const money = (microSar: number) => formatMicroSar(microSar, i18n.language);
  const figures = balance.data
    ? [
        {
          label: t("admin:organizations.organization.credits.available"),
          value: balance.data.available,
        },
        {
          label: t("admin:organizations.organization.credits.balance"),
          value: balance.data.balance,
        },
        {
          label: t("admin:organizations.organization.credits.held"),
          value: balance.data.held,
        },
      ]
    : [];

  return (
    <div className="flex flex-col gap-6">
      {balance.isPending ? (
        <Skeleton className="h-14 w-full" />
      ) : (
        <dl className="grid gap-4 sm:grid-cols-3">
          {figures.map((figure) => (
            <div key={figure.label}>
              <dt className="text-muted-foreground text-sm">{figure.label}</dt>
              <dd className="text-xl font-semibold tabular-nums">
                {money(figure.value)}
              </dd>
            </div>
          ))}
        </dl>
      )}

      {canAdjust && (
        <form
          id="admin-credits-adjust-form"
          noValidate
          onSubmit={form.handleSubmit((input) => adjust.mutateAsync(input))}
          className="flex flex-wrap items-start gap-4"
        >
          <Controller
            name="amountSar"
            control={form.control}
            render={({ field, fieldState }) => (
              <Field data-invalid={fieldState.invalid} className="w-48">
                <FieldLabel htmlFor="admin-credits-amount">
                  {t("admin:organizations.organization.credits.amount.label")}
                </FieldLabel>
                <div className="flex items-center gap-2">
                  <span className="text-muted-foreground">SAR</span>
                  <Input
                    id="admin-credits-amount"
                    inputMode="decimal"
                    placeholder={t(
                      "admin:organizations.organization.credits.amount.placeholder",
                    )}
                    {...field}
                    aria-invalid={fieldState.invalid}
                    disabled={form.formState.isSubmitting}
                  />
                </div>
                {fieldState.invalid && (
                  <FieldError errors={[fieldState.error]} />
                )}
              </Field>
            )}
          />
          <Controller
            name="reason"
            control={form.control}
            render={({ field, fieldState }) => (
              <Field
                data-invalid={fieldState.invalid}
                className="min-w-64 flex-1"
              >
                <FieldLabel htmlFor="admin-credits-reason">
                  {t("admin:organizations.organization.credits.reason.label")}
                </FieldLabel>
                <Input
                  id="admin-credits-reason"
                  placeholder={t(
                    "admin:organizations.organization.credits.reason.placeholder",
                  )}
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
            disabled={form.formState.isSubmitting}
          >
            {form.formState.isSubmitting ? (
              <Spinner />
            ) : (
              t("admin:organizations.organization.credits.submit")
            )}
          </Button>
          <FieldDescription className="w-full">
            {t("admin:organizations.organization.credits.description")}
          </FieldDescription>
        </form>
      )}
    </div>
  );
};
