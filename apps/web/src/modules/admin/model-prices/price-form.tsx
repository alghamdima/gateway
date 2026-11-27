"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { handle } from "@workspace/api/utils";
import { useTranslation } from "@workspace/i18n";
import { Button } from "@workspace/ui-web/button";
import { Checkbox } from "@workspace/ui-web/checkbox";
import { Input } from "@workspace/ui-web/input";
import { Label } from "@workspace/ui-web/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@workspace/ui-web/select";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@workspace/ui-web/sheet";
import { Spinner } from "@workspace/ui-web/spinner";
import { Textarea } from "@workspace/ui-web/textarea";
import { toast } from "@workspace/ui-web/toast";

import { api } from "~/lib/api/client";
import {
  isPlainDecimal,
  perMillion,
  perToken,
  TYPED_RATES,
} from "~/modules/admin/model-prices/format";

import type { TypedRateColumn } from "~/modules/admin/model-prices/format";

/** What the form starts from: a target, and the rates in force there (or none). */
export interface PriceFormStart {
  provider: string;
  model: string;
  providerKeyId: string | null;
  rates: Record<TypedRateColumn, string | null> & {
    otherRates: Record<string, string>;
  };
  /** Editing a price that exists: provider and model are fixed. */
  isExisting: boolean;
}

interface PriceFormProps {
  readonly start: PriceFormStart | null;
  readonly onClose: () => void;
}

const ALL_KEYS = "all";

interface OtherRow {
  field: string;
  value: string;
}

/**
 * A new version of a price (ADMIN-09): never an edit of the old one. Token
 * rates are typed per million tokens, other units per unit; the version is in
 * force from the time given, or now. A key's price is that key's whole price:
 * the form starts from the provider's rates.
 */
export const PriceForm = ({ start, onClose }: PriceFormProps) => (
  <Sheet open={start !== null} onOpenChange={(open) => !open && onClose()}>
    <SheetContent className="w-full overflow-y-auto sm:max-w-xl">
      {start && <PriceFormBody start={start} onClose={onClose} />}
    </SheetContent>
  </Sheet>
);

const PriceFormBody = ({
  start,
  onClose,
}: {
  start: PriceFormStart;
  onClose: () => void;
}) => {
  const { t } = useTranslation(["common", "admin"]);
  const queryClient = useQueryClient();
  const [provider, setProvider] = useState(start.provider);
  const [model, setModel] = useState(start.model);
  const [keyId, setKeyId] = useState(start.providerKeyId ?? ALL_KEYS);
  const [effectiveFrom, setEffectiveFrom] = useState("");
  const [isRemoved, setIsRemoved] = useState(false);
  const [note, setNote] = useState("");
  const [typed, setTyped] = useState<Record<TypedRateColumn, string>>(() => {
    const values: Record<TypedRateColumn, string> = {
      inputUsd: "",
      outputUsd: "",
      cacheReadUsd: "",
      cacheWriteUsd: "",
      cacheWrite1hUsd: "",
    };
    for (const { column } of TYPED_RATES) {
      const value = start.rates[column];
      values[column] = value === null ? "" : perMillion(value);
    }
    return values;
  });
  const [other, setOther] = useState<OtherRow[]>(
    Object.entries(start.rates.otherRates).map(([field, value]) => ({
      field,
      value,
    })),
  );

  const accounts = useQuery({
    queryKey: ["admin", "providers", "accounts"],
    queryFn: () => handle(api.admin.providers.accounts.$get)(),
  });
  const keys =
    accounts.data?.find((account) => account.name === provider)?.keys ?? [];
  const keyItems = [
    { value: ALL_KEYS, label: t("admin:modelPrices.form.allKeys") },
    ...keys.map((key) => ({ value: key.id, label: `${key.name} (${key.id})` })),
  ];

  const invalid = [
    ...TYPED_RATES.filter(
      ({ column }) => typed[column] !== "" && !isPlainDecimal(typed[column]),
    ).map(({ column }) => column),
    ...other
      .filter((row) => row.value !== "" && !isPlainDecimal(row.value))
      .map((row) => row.field),
  ];

  const save = useMutation({
    mutationFn: () => {
      const providerKeyId = keyId === ALL_KEYS ? null : keyId;
      const rate = (column: TypedRateColumn) =>
        typed[column] === "" ? null : perToken(typed[column]);
      const rates = {
        inputUsd: rate("inputUsd"),
        outputUsd: rate("outputUsd"),
        cacheReadUsd: rate("cacheReadUsd"),
        cacheWriteUsd: rate("cacheWriteUsd"),
        cacheWrite1hUsd: rate("cacheWrite1hUsd"),
      };
      return handle(api.admin["model-prices"].$post)({
        json: {
          provider: provider.trim(),
          model: model.trim(),
          providerKeyId,
          ...(effectiveFrom
            ? { effectiveFrom: new Date(effectiveFrom).toISOString() }
            : {}),
          isRemoved,
          ...(isRemoved
            ? {}
            : {
                rates: {
                  ...rates,
                  otherRates: Object.fromEntries(
                    other
                      .filter((row) => row.field.trim() && row.value.trim())
                      .map((row) => [row.field.trim(), row.value.trim()]),
                  ),
                },
              }),
          note,
        },
      });
    },
    onSuccess: async () => {
      await queryClient.invalidateQueries({
        queryKey: ["admin", "model-prices"],
      });
      toast.add({ title: t("admin:modelPrices.form.saved"), type: "success" });
      onClose();
    },
    onError: (error) => toast.add({ title: error.message, type: "error" }),
  });

  return (
    <form
      className="flex flex-col gap-4"
      onSubmit={(event) => {
        event.preventDefault();
        save.mutate();
      }}
    >
      <SheetHeader>
        <SheetTitle>
          {t(
            start.isExisting
              ? "admin:modelPrices.form.newVersion"
              : "admin:modelPrices.form.newPrice",
          )}
        </SheetTitle>
        <SheetDescription>
          {t("admin:modelPrices.form.description")}
        </SheetDescription>
      </SheetHeader>

      <div className="flex flex-col gap-4 px-4">
        <div className="grid grid-cols-2 gap-3">
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="price-provider">
              {t("admin:modelPrices.list.provider")}
            </Label>
            <Input
              id="price-provider"
              value={provider}
              disabled={start.isExisting}
              onChange={(event) => setProvider(event.target.value)}
              required
            />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="price-model">
              {t("admin:modelPrices.list.model")}
            </Label>
            <Input
              id="price-model"
              value={model}
              disabled={start.isExisting}
              onChange={(event) => setModel(event.target.value)}
              required
            />
          </div>
        </div>

        <div className="flex flex-col gap-1.5">
          <Label>{t("admin:modelPrices.list.key")}</Label>
          <Select
            items={keyItems}
            value={keyId}
            onValueChange={(value) =>
              setKeyId(typeof value === "string" ? value : ALL_KEYS)
            }
          >
            <SelectTrigger aria-label={t("admin:modelPrices.list.key")}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {keyItems.map((item) => (
                <SelectItem key={item.value} value={item.value}>
                  {item.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <p className="text-muted-foreground text-xs">
            {t("admin:modelPrices.form.keyHint")}
          </p>
        </div>

        {keyId !== ALL_KEYS && (
          <Label className="flex items-center gap-2">
            <Checkbox
              checked={isRemoved}
              onCheckedChange={(checked) => setIsRemoved(checked)}
            />
            {t("admin:modelPrices.form.remove")}
          </Label>
        )}

        <div className="flex flex-col gap-1.5">
          <Label htmlFor="price-from">{t("admin:modelPrices.form.from")}</Label>
          <Input
            id="price-from"
            type="datetime-local"
            value={effectiveFrom}
            onChange={(event) => setEffectiveFrom(event.target.value)}
          />
          <p className="text-muted-foreground text-xs">
            {t("admin:modelPrices.form.fromHint")}
          </p>
        </div>

        {!isRemoved && (
          <>
            <fieldset className="grid grid-cols-2 gap-3">
              <legend className="mb-2 text-sm font-medium">
                {t("admin:modelPrices.form.tokenRates")}
              </legend>
              {TYPED_RATES.map(({ column }) => (
                <div key={column} className="flex flex-col gap-1.5">
                  <Label htmlFor={`price-${column}`}>
                    {t(`admin:modelPrices.rate.${column}`)}
                  </Label>
                  <Input
                    id={`price-${column}`}
                    inputMode="decimal"
                    placeholder="—"
                    value={typed[column]}
                    aria-invalid={invalid.includes(column)}
                    onChange={(event) =>
                      setTyped({ ...typed, [column]: event.target.value })
                    }
                  />
                </div>
              ))}
            </fieldset>

            <fieldset className="flex flex-col gap-2">
              <legend className="mb-2 text-sm font-medium">
                {t("admin:modelPrices.form.otherRates")}
              </legend>
              <p className="text-muted-foreground text-xs">
                {t("admin:modelPrices.form.otherRatesHint")}
              </p>
              {other.map((row, index) => (
                <div key={index} className="flex gap-2">
                  <Input
                    aria-label={t("admin:modelPrices.form.unit")}
                    className="font-mono text-xs"
                    placeholder="output_cost_per_image"
                    value={row.field}
                    onChange={(event) =>
                      setOther(
                        other.map((item, at) =>
                          at === index
                            ? { ...item, field: event.target.value }
                            : item,
                        ),
                      )
                    }
                  />
                  <Input
                    aria-label={t("admin:modelPrices.form.usdPerUnit")}
                    className="w-40"
                    inputMode="decimal"
                    value={row.value}
                    aria-invalid={invalid.includes(row.field)}
                    onChange={(event) =>
                      setOther(
                        other.map((item, at) =>
                          at === index
                            ? { ...item, value: event.target.value }
                            : item,
                        ),
                      )
                    }
                  />
                  <Button
                    type="button"
                    variant="ghost"
                    onClick={() =>
                      setOther(other.filter((_, at) => at !== index))
                    }
                  >
                    {t("admin:modelPrices.form.removeUnit")}
                  </Button>
                </div>
              ))}
              <Button
                type="button"
                variant="outline"
                className="self-start"
                onClick={() => setOther([...other, { field: "", value: "" }])}
              >
                {t("admin:modelPrices.form.addUnit")}
              </Button>
            </fieldset>
          </>
        )}

        <div className="flex flex-col gap-1.5">
          <Label htmlFor="price-note">{t("admin:modelPrices.form.note")}</Label>
          <Textarea
            id="price-note"
            maxLength={500}
            value={note}
            onChange={(event) => setNote(event.target.value)}
          />
        </div>
      </div>

      <SheetFooter>
        <Button type="submit" disabled={save.isPending || invalid.length > 0}>
          {save.isPending && <Spinner />}
          {t("admin:modelPrices.form.save")}
        </Button>
      </SheetFooter>
    </form>
  );
};
