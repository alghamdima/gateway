"use client";

import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { parseAsStringLiteral, useQueryState } from "nuqs";

import { handle } from "@workspace/api/utils";
import { toMemberRole } from "@workspace/auth";
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

import { api } from "~/lib/api/client";
import { authClient } from "~/lib/auth/client";
import { CreditLots } from "~/modules/credits/credit-lots";
import { LowBalanceAlert } from "~/modules/credits/low-balance-alert";
import { TopUp } from "~/modules/credits/top-up";
import { formatMicroSar } from "~/modules/keys/money";
import { useActiveOrganization } from "~/modules/organization/hooks/use-active-organization";
import { RedeemCode } from "~/modules/promo-codes/redeem-code";

const PAGE_SIZE = 25;
/** While back from a payment, look for the webhook's credit this often. */
const AFTER_PAYMENT_REFRESH_MS = 3_000;

interface CreditsPageProps {
  readonly organizationId: string;
}

/** The prepaid balance and every entry in the organization's ledger (ADR 0001). */
export const CreditsPage = ({ organizationId }: CreditsPageProps) => {
  const { t, i18n } = useTranslation(["common", "organization"]);
  const { activeMember } = useActiveOrganization();
  // owners and admins pay; the API checks the same
  const canTopUp = authClient.organization.checkRolePermission({
    permissions: { billing: ["create"] },
    role: toMemberRole(activeMember?.role),
  });
  // set by the payment page when it sends the customer back
  const [returned, setReturned] = useQueryState(
    "top_up",
    parseAsStringLiteral(["success", "cancelled"]),
  );
  const refetchInterval =
    returned === "success" ? AFTER_PAYMENT_REFRESH_MS : false;

  const balance = useQuery({
    refetchInterval,
    queryKey: ["credits", organizationId, "balance"],
    queryFn: () =>
      handle(api.credits[":organizationId"].$get)({
        param: { organizationId },
      }),
  });

  const entries = useInfiniteQuery({
    refetchInterval,
    queryKey: ["credits", organizationId, "entries"],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) =>
      handle(api.credits[":organizationId"].entries.$get)({
        param: { organizationId },
        query: {
          limit: String(PAGE_SIZE),
          ...(pageParam ? { before: pageParam } : {}),
        },
      }),
    getNextPageParam: (lastPage) =>
      lastPage.length === PAGE_SIZE ? lastPage.at(-1)?.id : undefined,
  });

  const money = (microSar: number) => formatMicroSar(microSar, i18n.language);
  const rows = entries.data?.pages.flat() ?? [];
  const figures = balance.data
    ? [
        {
          label: t("organization:credits.available"),
          value: balance.data.available,
        },
        {
          label: t("organization:credits.balance"),
          value: balance.data.balance,
        },
        { label: t("organization:credits.held"), value: balance.data.held },
      ]
    : [];

  return (
    <div className="flex w-full flex-col gap-6">
      {returned && (
        <Alert>
          <AlertTitle>
            {t(`organization:credits.topUp.returned.${returned}.title`)}
          </AlertTitle>
          <AlertDescription className="flex flex-wrap items-center justify-between gap-2">
            {t(`organization:credits.topUp.returned.${returned}.description`)}
            <Button
              variant="outline"
              size="sm"
              onClick={() => setReturned(null)}
            >
              {t("common:close")}
            </Button>
          </AlertDescription>
        </Alert>
      )}
      <Card>
        <CardHeader>
          <CardTitle>{t("organization:credits.title")}</CardTitle>
          <CardDescription>
            {t("organization:credits.description")}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {balance.isPending ? (
            <Skeleton className="h-16 w-full" />
          ) : (
            <dl className="grid gap-4 sm:grid-cols-3">
              {figures.map((figure) => (
                <div key={figure.label}>
                  <dt className="text-muted-foreground text-sm">
                    {figure.label}
                  </dt>
                  <dd className="text-2xl font-semibold tabular-nums">
                    {money(figure.value)}
                  </dd>
                </div>
              ))}
            </dl>
          )}
        </CardContent>
      </Card>

      <CreditLots organizationId={organizationId} canRefund={canTopUp} />

      {canTopUp && <TopUp organizationId={organizationId} />}

      {canTopUp && <RedeemCode organizationId={organizationId} />}

      <LowBalanceAlert organizationId={organizationId} />

      <Card>
        <CardHeader>
          <CardTitle>{t("organization:credits.history.title")}</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          {entries.isPending && <Skeleton className="h-32 w-full" />}
          {!entries.isPending && rows.length === 0 && (
            <p className="text-muted-foreground text-sm">
              {t("organization:credits.history.empty")}
            </p>
          )}
          {rows.length > 0 && (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>
                    {t("organization:credits.history.when")}
                  </TableHead>
                  <TableHead>
                    {t("organization:credits.history.kind")}
                  </TableHead>
                  <TableHead className="text-right">
                    {t("organization:credits.history.amount")}
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((row) => (
                  <TableRow key={row.id}>
                    <TableCell className="text-muted-foreground whitespace-nowrap">
                      {new Date(row.createdAt).toLocaleString(i18n.language)}
                    </TableCell>
                    <TableCell>
                      {t(`organization:credits.kinds.${row.kind}`)}
                    </TableCell>
                    <TableCell
                      className={`text-right tabular-nums ${row.amount < 0 ? "" : "text-green-600"}`}
                    >
                      {row.amount > 0 ? "+" : ""}
                      {money(row.amount)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
          {entries.hasNextPage && (
            <Button
              variant="outline"
              onClick={() => void entries.fetchNextPage()}
              disabled={entries.isFetchingNextPage}
            >
              {entries.isFetchingNextPage ? (
                <Spinner />
              ) : (
                t("organization:credits.history.more")
              )}
            </Button>
          )}
        </CardContent>
      </Card>
    </div>
  );
};
