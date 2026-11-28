"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { handle } from "@workspace/api/utils";
import { useTranslation } from "@workspace/i18n";
import { Badge } from "@workspace/ui-web/badge";
import { Button } from "@workspace/ui-web/button";
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

/**
 * Whether the gateway charges our prices (ADR 0018): every difference between
 * its pricing overrides, its catalog and our table. Pushing mends our own
 * overrides; someone else's override is only reported.
 */
export const GatewayCheck = ({ canEdit }: { readonly canEdit: boolean }) => {
  const { t, i18n } = useTranslation(["common", "admin"]);
  const queryClient = useQueryClient();
  const check = useQuery({
    queryKey: ["admin", "model-prices", "check"],
    queryFn: () => handle(api.admin["model-prices"].check.$get)(),
  });
  const pushNow = useMutation({
    mutationFn: () => handle(api.admin["model-prices"].push.$post)(),
    onSuccess: async () => {
      await queryClient.invalidateQueries({
        queryKey: ["admin", "model-prices"],
      });
      toast.add({
        title: t("admin:modelPrices.check.pushed"),
        type: "success",
      });
    },
    onError: (error) => toast.add({ title: error.message, type: "error" }),
  });

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="text-muted-foreground text-sm">
          {check.data
            ? t("admin:modelPrices.check.summary", {
                targets: check.data.targets,
                drift: check.data.drift.length,
                at: new Date(check.data.checkedAt).toLocaleString(
                  i18n.language,
                ),
              })
            : t("admin:modelPrices.check.description")}
        </p>
        <div className="flex gap-2">
          <Button
            variant="outline"
            disabled={check.isFetching}
            onClick={() => void check.refetch()}
          >
            {t("admin:modelPrices.check.run")}
          </Button>
          {canEdit && (
            <Button
              disabled={pushNow.isPending}
              onClick={() => pushNow.mutate()}
            >
              {t("admin:modelPrices.check.push")}
            </Button>
          )}
        </div>
      </div>
      {check.isPending && <Skeleton className="h-24 w-full" />}
      {check.isError && (
        <p role="alert" className="text-destructive text-sm">
          {check.error.message}
        </p>
      )}
      {check.data?.drift.length === 0 && (
        <p className="text-sm">{t("admin:modelPrices.check.clean")}</p>
      )}
      {check.data && check.data.drift.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t("admin:modelPrices.check.kind")}</TableHead>
              <TableHead>{t("admin:modelPrices.list.model")}</TableHead>
              <TableHead>{t("admin:modelPrices.list.key")}</TableHead>
              <TableHead>{t("admin:modelPrices.check.detail")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {check.data.drift.map((item, index) => (
              <TableRow
                key={`${item.kind}-${item.provider}-${item.model}-${index}`}
              >
                <TableCell>
                  <Badge variant="destructive">
                    {t(`admin:modelPrices.drift.${item.kind}`)}
                  </Badge>
                </TableCell>
                <TableCell className="font-mono text-xs">
                  {item.provider}/{item.model}
                </TableCell>
                <TableCell className="font-mono text-xs">
                  {item.providerKeyId ?? "—"}
                </TableCell>
                <TableCell className="text-xs">{item.detail}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </div>
  );
};
