"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { handle } from "@workspace/api/utils";
import { useTranslation } from "@workspace/i18n";
import { Button } from "@workspace/ui-web/button";
import { Input } from "@workspace/ui-web/input";
import { Skeleton } from "@workspace/ui-web/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@workspace/ui-web/table";
import { toast } from "@workspace/ui-web/toast";

import { api } from "~/lib/api/client";
import { perMillion, TYPED_RATES } from "~/modules/admin/model-prices/format";

import type { TypedRateColumn } from "~/modules/admin/model-prices/format";

type RateValues = Record<TypedRateColumn, string | null> & {
  otherRates: Record<string, string>;
};

/** One line per rate that differs: ours, then the source's. */
const differences = (base: RateValues | null, proposed: RateValues) => {
  const lines: { label: string; from: string; to: string }[] = [];
  for (const { column } of TYPED_RATES) {
    const before = base?.[column] ?? null;
    const after = proposed[column];
    if (before !== after) {
      lines.push({
        label: column,
        from: before === null ? "—" : `${perMillion(before)} / 1M`,
        to: after === null ? "—" : `${perMillion(after)} / 1M`,
      });
    }
  }
  const fields = new Set([
    ...Object.keys(base?.otherRates ?? {}),
    ...Object.keys(proposed.otherRates),
  ]);
  for (const field of fields) {
    const before = base?.otherRates[field];
    const after = proposed.otherRates[field];
    if (before !== after) {
      lines.push({ label: field, from: before ?? "—", to: after ?? "—" });
    }
  }
  return lines;
};

const onError = (error: Error) =>
  toast.add({ title: error.message, type: "error" });

/**
 * Prices the datasheet proposes (ADR 0018): each beside the price it would
 * replace. Approving makes it a new version; rejecting, with a reason, leaves
 * the price as it is. A sync never changes a price by itself.
 */
export const SuggestionsInbox = ({
  canEdit,
}: {
  readonly canEdit: boolean;
}) => {
  const { t, i18n } = useTranslation(["common", "admin"]);
  const queryClient = useQueryClient();
  const [reasons, setReasons] = useState<Record<number, string>>({});
  const suggestions = useQuery({
    queryKey: ["admin", "model-prices", "suggestions"],
    queryFn: () =>
      handle(api.admin["model-prices"].suggestions.$get)({
        query: { status: "pending" },
      }),
  });
  const refresh = () =>
    queryClient.invalidateQueries({ queryKey: ["admin", "model-prices"] });

  const sync = useMutation({
    mutationFn: () =>
      handle(api.admin["model-prices"].suggestions.sync.$post)(),
    onSuccess: async (result) => {
      await refresh();
      toast.add({
        title: t("admin:modelPrices.suggestions.synced", {
          created: result.created,
        }),
        type: "success",
      });
    },
    onError,
  });
  const approve = useMutation({
    mutationFn: (id: number) =>
      handle(api.admin["model-prices"].suggestions[":id"].approve.$post)({
        param: { id: String(id) },
        json: { reason: reasons[id] ?? "" },
      }),
    onSuccess: async () => {
      await refresh();
      toast.add({
        title: t("admin:modelPrices.suggestions.approved"),
        type: "success",
      });
    },
    onError,
  });
  const reject = useMutation({
    mutationFn: (id: number) =>
      handle(api.admin["model-prices"].suggestions[":id"].reject.$post)({
        param: { id: String(id) },
        json: { reason: reasons[id] ?? "" },
      }),
    onSuccess: async () => {
      await refresh();
      toast.add({
        title: t("admin:modelPrices.suggestions.rejected"),
        type: "success",
      });
    },
    onError,
  });

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="text-muted-foreground text-sm">
          {t("admin:modelPrices.suggestions.description")}
        </p>
        {canEdit && (
          <Button
            variant="outline"
            disabled={sync.isPending}
            onClick={() => sync.mutate()}
          >
            {t("admin:modelPrices.suggestions.sync")}
          </Button>
        )}
      </div>
      {suggestions.isPending && <Skeleton className="h-32 w-full" />}
      {suggestions.data?.length === 0 && (
        <p className="text-muted-foreground text-sm">
          {t("admin:modelPrices.suggestions.empty")}
        </p>
      )}
      {suggestions.data && suggestions.data.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t("admin:modelPrices.list.model")}</TableHead>
              <TableHead>{t("admin:modelPrices.suggestions.change")}</TableHead>
              <TableHead>
                {t("admin:modelPrices.suggestions.proposed")}
              </TableHead>
              {canEdit && (
                <TableHead>
                  {t("admin:modelPrices.suggestions.decide")}
                </TableHead>
              )}
            </TableRow>
          </TableHeader>
          <TableBody>
            {suggestions.data.map((suggestion) => (
              <TableRow key={suggestion.id}>
                <TableCell className="font-mono text-xs">
                  {suggestion.provider}/{suggestion.model}
                  {!suggestion.base && (
                    <p className="text-muted-foreground font-sans">
                      {t("admin:modelPrices.suggestions.newModel")}
                    </p>
                  )}
                </TableCell>
                <TableCell className="font-mono text-xs">
                  {differences(suggestion.base, suggestion).map((line) => (
                    <div key={line.label}>
                      {line.label}: {line.from} → {line.to}
                    </div>
                  ))}
                </TableCell>
                <TableCell className="text-xs whitespace-nowrap">
                  {new Date(suggestion.createdAt).toLocaleString(i18n.language)}
                </TableCell>
                {canEdit && (
                  <TableCell>
                    <div className="flex flex-col gap-2">
                      <Input
                        aria-label={t("admin:modelPrices.suggestions.reason")}
                        placeholder={t("admin:modelPrices.suggestions.reason")}
                        value={reasons[suggestion.id] ?? ""}
                        onChange={(event) =>
                          setReasons({
                            ...reasons,
                            [suggestion.id]: event.target.value,
                          })
                        }
                      />
                      <div className="flex gap-2">
                        <Button
                          size="sm"
                          disabled={approve.isPending}
                          onClick={() => approve.mutate(suggestion.id)}
                        >
                          {t("admin:modelPrices.suggestions.approve")}
                        </Button>
                        <Button
                          size="sm"
                          variant="outline"
                          disabled={
                            reject.isPending ||
                            !(reasons[suggestion.id] ?? "").trim()
                          }
                          onClick={() => reject.mutate(suggestion.id)}
                        >
                          {t("admin:modelPrices.suggestions.reject")}
                        </Button>
                      </div>
                    </div>
                  </TableCell>
                )}
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </div>
  );
};
