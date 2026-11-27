"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
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
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@workspace/ui-web/dialog";
import { Skeleton } from "@workspace/ui-web/skeleton";
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

import { api } from "~/lib/api/client";
import { formatMicroSar } from "~/modules/keys/money";

interface CreditLotsProps {
  readonly organizationId: string;
  /** Owners and admins may refund, as they may pay; the API checks the same. */
  readonly canRefund: boolean;
}

interface RefundTarget {
  id: string;
  amountMicroSar: number;
  grantedMicroSar: number;
  /** The unused promo bonus the refund takes back with it (ADR 0019). */
  bonusClawbackMicroSar: number;
}

/**
 * The organization's credit, purchase by purchase (BILL-17, BILL-10): what is
 * left of each, when it expires, a notice before credit expires, and the
 * refund of a top-up within its window (the unused part only, never the fee;
 * the unused part of its promo bonus is removed with it, and the dialog says how much).
 */
export const CreditLots = ({ organizationId, canRefund }: CreditLotsProps) => {
  const { t, i18n } = useTranslation(["common", "organization"]);
  const queryClient = useQueryClient();
  const [target, setTarget] = useState<RefundTarget | null>(null);

  const lots = useQuery({
    queryKey: ["credits", organizationId, "lots"],
    queryFn: () =>
      handle(api.credits[":organizationId"].lots.$get)({
        param: { organizationId },
      }),
  });

  const refund = useMutation({
    mutationFn: (lotId: string) =>
      handle(api.credits[":organizationId"].lots[":lotId"].refund.$post)({
        param: { organizationId, lotId },
      }),
    onSuccess: async (result) => {
      setTarget(null);
      await queryClient.invalidateQueries({
        queryKey: ["credits", organizationId],
      });
      toast.add({
        title: t(`organization:credits.lots.refund.done.${result.status}`, {
          amount: money(result.amountMicroSar),
        }),
        type: "success",
      });
    },
    onError: () => setTarget(null),
  });

  const money = (microSar: number) => formatMicroSar(microSar, i18n.language);
  const date = (iso: string) =>
    new Date(iso).toLocaleDateString(i18n.language, { dateStyle: "medium" });
  const dateTime = (iso: string) =>
    new Date(iso).toLocaleString(i18n.language, {
      dateStyle: "medium",
      timeStyle: "short",
    });

  if (lots.isPending) {
    return <Skeleton className="h-32 w-full" />;
  }
  if (!lots.data || lots.data.lots.length === 0) {
    return null;
  }
  const { expiring } = lots.data;

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("organization:credits.lots.title")}</CardTitle>
        <CardDescription>
          {t("organization:credits.lots.description")}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {expiring && (
          <Alert variant={expiring.withinDays <= 7 ? "destructive" : "default"}>
            <AlertTitle>
              {t("organization:credits.lots.expiring.title", {
                amount: money(expiring.amountMicroSar),
                days: expiring.withinDays,
              })}
            </AlertTitle>
            <AlertDescription>
              {t("organization:credits.lots.expiring.description", {
                date: date(expiring.firstExpiresAt),
              })}
            </AlertDescription>
          </Alert>
        )}
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t("organization:credits.lots.added")}</TableHead>
              <TableHead>{t("organization:credits.lots.source")}</TableHead>
              <TableHead className="text-right">
                {t("organization:credits.lots.remaining")}
              </TableHead>
              <TableHead>{t("organization:credits.lots.expires")}</TableHead>
              <TableHead className="text-right">
                <span className="sr-only">
                  {t("organization:credits.lots.refund.label")}
                </span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {lots.data.lots.map((lot) => (
              <TableRow key={lot.id}>
                <TableCell className="text-muted-foreground whitespace-nowrap">
                  {date(lot.createdAt)}
                </TableCell>
                <TableCell>
                  {t(`organization:credits.lots.sources.${lot.source}`)}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {money(lot.remainingMicroSar)}
                  <span className="text-muted-foreground">
                    {" "}
                    / {money(lot.grantedMicroSar)}
                  </span>
                </TableCell>
                <TableCell className="whitespace-nowrap">
                  {lot.expiresAt
                    ? date(lot.expiresAt)
                    : t("organization:credits.lots.never")}
                </TableCell>
                <TableCell className="text-right">
                  {lot.refund.state === "available" &&
                    canRefund &&
                    lot.refund.refundableUntil && (
                      <Button
                        variant="outline"
                        size="sm"
                        title={t("organization:credits.lots.refund.until", {
                          date: dateTime(lot.refund.refundableUntil),
                        })}
                        onClick={() =>
                          setTarget({
                            id: lot.id,
                            amountMicroSar: lot.refund.amountMicroSar,
                            grantedMicroSar: lot.grantedMicroSar,
                            bonusClawbackMicroSar:
                              lot.refund.bonusClawbackMicroSar,
                          })
                        }
                      >
                        {t("organization:credits.lots.refund.label")}
                      </Button>
                    )}
                  {lot.refund.state !== "available" &&
                    lot.refund.state !== "none" && (
                      <span className="text-muted-foreground text-sm">
                        {t(
                          `organization:credits.lots.refund.states.${lot.refund.state}`,
                        )}
                      </span>
                    )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        {lots.data.areRefundsEnabled && (
          <p className="text-muted-foreground text-sm">
            {t("organization:credits.lots.refund.policy")}
          </p>
        )}
      </CardContent>

      <Dialog
        open={target !== null}
        onOpenChange={(open) => !open && setTarget(null)}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              {t("organization:credits.lots.refund.confirm.title")}
            </DialogTitle>
            <DialogDescription>
              {target &&
                t("organization:credits.lots.refund.confirm.description", {
                  amount: money(target.amountMicroSar),
                  granted: money(target.grantedMicroSar),
                })}
            </DialogDescription>
            {target && target.bonusClawbackMicroSar > 0 && (
              <DialogDescription>
                {t("organization:credits.lots.refund.confirm.bonus", {
                  bonus: money(target.bonusClawbackMicroSar),
                })}
              </DialogDescription>
            )}
          </DialogHeader>
          <DialogFooter>
            <DialogClose
              render={<Button variant="outline">{t("common:cancel")}</Button>}
            />
            <Button
              variant="destructive"
              disabled={refund.isPending}
              onClick={() => target && refund.mutate(target.id)}
            >
              {refund.isPending ? (
                <Spinner />
              ) : (
                t("organization:credits.lots.refund.confirm.action", {
                  amount: target ? money(target.amountMicroSar) : "",
                })
              )}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  );
};
