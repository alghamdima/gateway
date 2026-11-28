"use client";

import { useQuery } from "@tanstack/react-query";

import { handle } from "@workspace/api/utils";
import { useTranslation } from "@workspace/i18n";
import { Badge } from "@workspace/ui-web/badge";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@workspace/ui-web/sheet";
import { Skeleton } from "@workspace/ui-web/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@workspace/ui-web/table";

import { api } from "~/lib/api/client";
import {
  isTokenField,
  perMillion,
  TYPED_RATES,
} from "~/modules/admin/model-prices/format";

interface HistorySheetProps {
  readonly target: { provider: string; model: string } | null;
  readonly onClose: () => void;
}

/** Every version of one model's price, its keys' included, newest first; none is ever edited. */
export const HistorySheet = ({ target, onClose }: HistorySheetProps) => {
  const { t, i18n } = useTranslation(["common", "admin"]);
  const history = useQuery({
    queryKey: ["admin", "model-prices", "history", target],
    enabled: target !== null,
    queryFn: () =>
      handle(api.admin["model-prices"].history.$get)({
        query: { provider: target?.provider ?? "", model: target?.model ?? "" },
      }),
  });

  return (
    <Sheet open={target !== null} onOpenChange={(open) => !open && onClose()}>
      <SheetContent className="w-full sm:max-w-3xl">
        <SheetHeader>
          <SheetTitle className="font-mono">
            {target ? `${target.provider}/${target.model}` : ""}
          </SheetTitle>
          <SheetDescription>
            {t("admin:modelPrices.history.description")}
          </SheetDescription>
        </SheetHeader>
        <div className="overflow-auto px-4 pb-4">
          {history.isPending && <Skeleton className="h-40 w-full" />}
          {history.data && (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t("admin:modelPrices.history.from")}</TableHead>
                  <TableHead>{t("admin:modelPrices.list.key")}</TableHead>
                  <TableHead>{t("admin:modelPrices.list.rates")}</TableHead>
                  <TableHead>{t("admin:modelPrices.history.source")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {history.data.map((version) => {
                  const from = new Date(version.effectiveFrom);
                  const typed = TYPED_RATES.flatMap(({ column, field }) => {
                    const value = version[column];
                    return value === null ? [] : [[field, value] as const];
                  });
                  const rates = [
                    ...typed,
                    ...Object.entries(version.otherRates),
                  ];
                  return (
                    <TableRow key={version.id}>
                      <TableCell className="whitespace-nowrap">
                        {from.toLocaleString(i18n.language)}
                        {version.isScheduled && (
                          <Badge variant="outline" className="ml-2">
                            {t("admin:modelPrices.history.scheduled")}
                          </Badge>
                        )}
                      </TableCell>
                      <TableCell className="font-mono text-xs">
                        {version.providerKeyId ??
                          t("admin:modelPrices.list.allKeys")}
                      </TableCell>
                      <TableCell className="font-mono text-xs">
                        {version.isRemoved
                          ? t("admin:modelPrices.history.removed")
                          : rates.map(([field, value]) => (
                              <div key={field}>
                                {field}:{" "}
                                {isTokenField(field)
                                  ? `${perMillion(value)} / 1M`
                                  : value}
                              </div>
                            ))}
                      </TableCell>
                      <TableCell>
                        {t(`admin:modelPrices.source.${version.source}`)}
                        {version.note && (
                          <p className="text-muted-foreground text-xs">
                            {version.note}
                          </p>
                        )}
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          )}
        </div>
      </SheetContent>
    </Sheet>
  );
};
