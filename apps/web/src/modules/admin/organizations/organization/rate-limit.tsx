"use client";

import { standardSchemaResolver } from "@hookform/resolvers/standard-schema";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
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

const toNumber = (value: string) => (value === "" ? null : Number(value));

interface OrganizationRateLimitProps {
  readonly organizationId: string;
}

/**
 * Requests and tokens per minute across all the organization's keys
 * (step G1). The gateway refuses what goes over with 429.
 */
export const OrganizationRateLimit = ({
  organizationId,
}: OrganizationRateLimitProps) => {
  const { t, i18n } = useTranslation(["common", "admin"]);
  const queryClient = useQueryClient();
  const canUpdate = useStaffPermission({ organizations: ["update"] });
  const queryKey = ["admin", "organizations", organizationId, "rate-limit"];

  const current = useQuery({
    queryKey,
    queryFn: () =>
      handle(api.admin.organizations[":id"]["rate-limit"].$get)({
        param: { id: organizationId },
      }),
  });

  const perMinute = (max: number) =>
    z
      .string()
      .trim()
      .refine(
        (value) =>
          value === "" ||
          (/^\d+$/.test(value) && Number(value) > 0 && Number(value) <= max),
        {
          message: t("admin:organizations.organization.rateLimit.invalid"),
        },
      );
  const schema = z.object({
    requests: perMinute(1_000_000),
    tokens: perMinute(10_000_000_000),
  });

  const form = useForm({
    resolver: standardSchemaResolver(schema),
    values: {
      requests: current.data?.requestsPerMinute?.toString() ?? "",
      tokens: current.data?.tokensPerMinute?.toString() ?? "",
    },
  });

  const save = useMutation({
    mutationFn: (input: {
      requestsPerMinute: number | null;
      tokensPerMinute: number | null;
    }) =>
      handle(api.admin.organizations[":id"]["rate-limit"].$put)({
        param: { id: organizationId },
        json: input,
      }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey });
      toast.add({
        title: t("admin:organizations.organization.rateLimit.saved"),
        type: "success",
      });
    },
  });

  const number = (value: number | null) =>
    value === null
      ? t("admin:organizations.organization.rateLimit.none")
      : new Intl.NumberFormat(i18n.language).format(value);

  if (current.isPending) {
    return <Skeleton className="h-14 w-full" />;
  }

  const figures = [
    {
      label: t("admin:organizations.organization.rateLimit.requests"),
      value: current.data?.requestsPerMinute ?? null,
    },
    {
      label: t("admin:organizations.organization.rateLimit.tokens"),
      value: current.data?.tokensPerMinute ?? null,
    },
  ];

  return (
    <div className="flex flex-col gap-6">
      <dl className="grid gap-4 sm:grid-cols-2">
        {figures.map((figure) => (
          <div key={figure.label}>
            <dt className="text-muted-foreground text-sm">{figure.label}</dt>
            <dd className="text-xl font-semibold tabular-nums">
              {number(figure.value)}
            </dd>
          </div>
        ))}
      </dl>

      {canUpdate && (
        <form
          id="admin-rate-limit-form"
          noValidate
          onSubmit={form.handleSubmit((values) =>
            save.mutateAsync({
              requestsPerMinute: toNumber(values.requests),
              tokensPerMinute: toNumber(values.tokens),
            }),
          )}
          className="flex flex-wrap items-start gap-4"
        >
          {(["requests", "tokens"] as const).map((name) => (
            <Controller
              key={name}
              name={name}
              control={form.control}
              render={({ field, fieldState }) => (
                <Field data-invalid={fieldState.invalid} className="w-56">
                  <FieldLabel htmlFor={`admin-rate-limit-${name}`}>
                    {t(`admin:organizations.organization.rateLimit.${name}`)}
                  </FieldLabel>
                  <Input
                    id={`admin-rate-limit-${name}`}
                    inputMode="numeric"
                    placeholder={t(
                      "admin:organizations.organization.rateLimit.none",
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
          ))}
          <Button
            type="submit"
            className="mt-6"
            disabled={form.formState.isSubmitting}
          >
            {form.formState.isSubmitting ? <Spinner /> : t("common:save")}
          </Button>
          <FieldDescription className="w-full">
            {t("admin:organizations.organization.rateLimit.description")}
          </FieldDescription>
        </form>
      )}
    </div>
  );
};
