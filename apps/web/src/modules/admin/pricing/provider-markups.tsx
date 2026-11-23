"use client";

import { standardSchemaResolver } from "@hookform/resolvers/standard-schema";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Controller, useForm } from "react-hook-form";
import * as z from "zod";

import { pricingProviderSchema } from "@workspace/api/schema";
import { useTranslation } from "@workspace/i18n";
import { cn } from "@workspace/ui";
import { Button } from "@workspace/ui-web/button";
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
} from "@workspace/ui-web/field";
import { Icons } from "@workspace/ui-web/icons";
import { Input } from "@workspace/ui-web/input";
import { Spinner } from "@workspace/ui-web/spinner";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@workspace/ui-web/table";
import { toast } from "@workspace/ui-web/toast";

import { admin } from "~/modules/admin/lib/api";
import {
  bpsToPercent,
  GATEWAY_PROVIDERS,
  percentSchema,
  percentToBps,
} from "~/modules/admin/pricing/rates";

interface ProviderMarkup {
  provider: string;
  markupBps: number;
  updatedAt: string;
}

interface ProviderMarkupsProps {
  readonly canEdit: boolean;
  readonly providers: ProviderMarkup[];
  readonly defaultMarkupBps: number;
}

export const ProviderMarkups = ({
  canEdit,
  providers,
  defaultMarkupBps,
}: ProviderMarkupsProps) => {
  const { t, i18n } = useTranslation(["common", "admin"]);
  const queryClient = useQueryClient();

  const schema = z.object({
    provider: pricingProviderSchema,
    markup: percentSchema(1000, t("admin:pricing.error.decimals")),
  });

  const form = useForm({
    resolver: standardSchemaResolver(schema),
    defaultValues: { provider: "", markup: Number.NaN },
  });

  const onChanged = async (title: string) => {
    await queryClient.invalidateQueries({ queryKey: ["admin", "pricing"] });
    toast.add({ title, type: "success" });
  };

  const setProvider = useMutation({
    ...admin.mutations.pricing.setProvider,
    onSuccess: async () => {
      form.reset();
      await onChanged(t("admin:pricing.saved"));
    },
  });

  const removeProvider = useMutation({
    ...admin.mutations.pricing.removeProvider,
    onSuccess: () => onChanged(t("admin:pricing.providers.removed")),
  });

  const onSubmit = async (data: z.infer<typeof schema>) => {
    await setProvider.mutateAsync({
      provider: data.provider,
      markupBps: percentToBps(data.markup),
    });
  };

  return (
    <div className="space-y-6">
      {providers.length > 0 ? (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t("admin:pricing.providers.provider")}</TableHead>
              <TableHead>{t("admin:pricing.providers.markup")}</TableHead>
              <TableHead>{t("admin:pricing.providers.updatedAt")}</TableHead>
              <TableHead className="w-0" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {providers.map((row) => (
              <TableRow key={row.provider}>
                <TableCell className="font-mono">{row.provider}</TableCell>
                <TableCell>{bpsToPercent(row.markupBps)}%</TableCell>
                <TableCell className="text-muted-foreground">
                  {new Date(row.updatedAt).toLocaleString(i18n.language)}
                </TableCell>
                <TableCell className={cn("flex gap-1", !canEdit && "hidden")}>
                  <Button
                    variant="ghost"
                    size="sm"
                    type="button"
                    onClick={() =>
                      form.reset({
                        provider: row.provider,
                        markup: bpsToPercent(row.markupBps),
                      })
                    }
                  >
                    {t("edit")}
                  </Button>
                  <Button
                    variant="ghost"
                    size="icon"
                    type="button"
                    aria-label={t("remove")}
                    disabled={removeProvider.isPending}
                    onClick={() =>
                      removeProvider.mutate({ provider: row.provider })
                    }
                  >
                    <Icons.Trash className="size-4" />
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      ) : (
        <p className="text-muted-foreground text-sm">
          {t("admin:pricing.providers.empty")}
        </p>
      )}

      {canEdit && (
        <form
          id="admin-pricing-provider-form"
          noValidate
          onSubmit={form.handleSubmit(onSubmit)}
          className="flex flex-wrap items-start gap-4"
        >
          <Controller
            name="provider"
            control={form.control}
            render={({ field, fieldState }) => (
              <Field data-invalid={fieldState.invalid} className="w-56">
                <FieldLabel htmlFor="admin-pricing-provider">
                  {t("admin:pricing.providers.provider")}
                </FieldLabel>
                <Input
                  id="admin-pricing-provider"
                  list="admin-pricing-provider-names"
                  autoComplete="off"
                  placeholder="openai"
                  {...field}
                  aria-invalid={fieldState.invalid}
                  disabled={form.formState.isSubmitting}
                />
                <datalist id="admin-pricing-provider-names">
                  {GATEWAY_PROVIDERS.map((name) => (
                    <option key={name} value={name}>
                      {name}
                    </option>
                  ))}
                </datalist>
                {fieldState.invalid && (
                  <FieldError errors={[fieldState.error]} />
                )}
              </Field>
            )}
          />
          <Controller
            name="markup"
            control={form.control}
            render={({ field, fieldState }) => (
              <Field data-invalid={fieldState.invalid} className="w-40">
                <FieldLabel htmlFor="admin-pricing-provider-markup">
                  {t("admin:pricing.providers.markup")}
                </FieldLabel>
                <div className="flex items-center gap-2">
                  <Input
                    id="admin-pricing-provider-markup"
                    type="number"
                    inputMode="decimal"
                    step="0.01"
                    min={0}
                    value={Number.isNaN(field.value) ? "" : field.value}
                    onChange={(event) =>
                      field.onChange(event.target.valueAsNumber)
                    }
                    onBlur={field.onBlur}
                    aria-invalid={fieldState.invalid}
                    disabled={form.formState.isSubmitting}
                  />
                  <span className="text-muted-foreground">%</span>
                </div>
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
            {form.formState.isSubmitting ? <Spinner /> : t("save")}
          </Button>
        </form>
      )}
      <FieldDescription>
        {t("admin:pricing.providers.description", {
          markup: bpsToPercent(defaultMarkupBps),
        })}
      </FieldDescription>
    </div>
  );
};
