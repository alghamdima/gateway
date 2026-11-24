"use client";

import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { useState } from "react";

import { handle } from "@workspace/api/utils";
import { useTranslation } from "@workspace/i18n";
import { Badge } from "@workspace/ui-web/badge";
import { Button } from "@workspace/ui-web/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui-web/card";
import {
  Modal,
  ModalBody,
  ModalContent,
  ModalDescription,
  ModalHeader,
  ModalTitle,
} from "@workspace/ui-web/modal";
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
import { formatMicroUsd, MICRO_USD } from "~/modules/keys/money";

const PAGE_SIZE = 25;

interface RecentRequestsProps {
  readonly organizationId: string;
}

/** The latest charged requests; one opens its record (step 9.2). */
export const RecentRequests = ({ organizationId }: RecentRequestsProps) => {
  const { t, i18n } = useTranslation(["common", "organization"]);
  const [openId, setOpenId] = useState<string | null>(null);

  const requests = useInfiniteQuery({
    queryKey: ["activity", organizationId, "requests"],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) =>
      handle(api.activity[":organizationId"].requests.$get)({
        param: { organizationId },
        query: {
          limit: String(PAGE_SIZE),
          ...(pageParam ? { before: pageParam } : {}),
        },
      }),
    getNextPageParam: (lastPage) =>
      lastPage.length === PAGE_SIZE ? lastPage.at(-1)?.createdAt : undefined,
  });

  const rows = requests.data?.pages.flat() ?? [];
  const when = new Intl.DateTimeFormat(i18n.language, {
    dateStyle: "medium",
    timeStyle: "medium",
  });
  const count = (value: number) =>
    new Intl.NumberFormat(i18n.language).format(value);

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("organization:activity.recent.title")}</CardTitle>
        <CardDescription>
          {t("organization:activity.recent.description")}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {requests.isPending && <Skeleton className="h-32 w-full" />}
        {!requests.isPending && rows.length === 0 && (
          <p className="text-muted-foreground text-sm">
            {t("organization:activity.empty")}
          </p>
        )}
        {rows.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("organization:activity.recent.when")}</TableHead>
                <TableHead>{t("organization:activity.model")}</TableHead>
                <TableHead>{t("organization:activity.key")}</TableHead>
                <TableHead className="text-right">
                  {t("organization:activity.tokens")}
                </TableHead>
                <TableHead className="text-right">
                  {t("organization:activity.spend")}
                </TableHead>
                <TableHead>
                  <span className="sr-only">
                    {t("organization:activity.recent.details")}
                  </span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((row, index) => (
                <TableRow
                  key={`${row.id ?? "none"}-${row.createdAt}-${index}`}
                  className={row.id ? "cursor-pointer" : undefined}
                  onClick={() => row.id && setOpenId(row.id)}
                >
                  <TableCell className="whitespace-nowrap">
                    {when.format(new Date(row.createdAt))}
                  </TableCell>
                  <TableCell>
                    {row.provider}/{row.model}
                    {row.failed && (
                      <Badge variant="destructive" className="ms-2">
                        {t("organization:activity.recent.failed")}
                      </Badge>
                    )}
                  </TableCell>
                  <TableCell>
                    {row.keyName ?? t("organization:activity.deletedKey")}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {count(row.promptTokens + row.completionTokens)}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {formatMicroUsd(row.cost, i18n.language)}
                  </TableCell>
                  <TableCell className="text-right">
                    {row.id && (
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={(event) => {
                          event.stopPropagation();
                          setOpenId(row.id);
                        }}
                      >
                        {t("organization:activity.recent.details")}
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        {requests.hasNextPage && (
          <Button
            variant="outline"
            onClick={() => requests.fetchNextPage()}
            disabled={requests.isFetchingNextPage}
          >
            {requests.isFetchingNextPage ? (
              <Spinner />
            ) : (
              t("organization:credits.history.more")
            )}
          </Button>
        )}
      </CardContent>
      {openId && (
        <RequestRecord
          organizationId={organizationId}
          id={openId}
          onClose={() => setOpenId(null)}
        />
      )}
    </Card>
  );
};

const RequestRecord = ({
  organizationId,
  id,
  onClose,
}: {
  readonly organizationId: string;
  readonly id: string;
  readonly onClose: () => void;
}) => {
  const { t, i18n } = useTranslation(["common", "organization"]);
  const record = useQuery({
    queryKey: ["activity", organizationId, "requests", id],
    queryFn: () =>
      handle(api.activity[":organizationId"].requests[":id"].$get)({
        param: { organizationId, id },
      }),
  });
  const usd = (value: number) =>
    formatMicroUsd(Math.round(value * MICRO_USD), i18n.language);
  const count = (value: number) =>
    new Intl.NumberFormat(i18n.language).format(value);
  const unknown = t("organization:activity.record.unknown");
  const data = record.data;
  const facts = data
    ? [
        {
          label: t("organization:activity.model"),
          value: `${data.provider_name ?? ""}/${data.model ?? ""}`,
        },
        {
          label: t("organization:activity.record.promptTokens"),
          value: count(data.tokens_prompt),
        },
        {
          label: t("organization:activity.record.completionTokens"),
          value: count(data.tokens_completion),
        },
        {
          label: t("organization:activity.spend"),
          value: usd(data.total_cost),
        },
        {
          label: t("organization:activity.record.latency"),
          value:
            data.latency === null
              ? unknown
              : `${count(Math.round(data.latency))} ms`,
        },
        {
          label: t("organization:activity.record.finishReason"),
          value: data.finish_reason ?? unknown,
        },
        {
          label: t("organization:activity.record.streamed"),
          value:
            data.streamed === null
              ? unknown
              : data.streamed
                ? t("organization:activity.record.yes")
                : t("organization:activity.record.no"),
        },
      ]
    : [];

  return (
    <Modal
      open
      drawer={{ onOpenChange: (open) => !open && onClose() }}
      dialog={{ onOpenChange: (open) => !open && onClose() }}
    >
      <ModalContent>
        <ModalHeader>
          <ModalTitle>{t("organization:activity.record.title")}</ModalTitle>
          <ModalDescription className="font-mono text-xs break-all">
            {id}
          </ModalDescription>
        </ModalHeader>
        <ModalBody className="space-y-6">
          {record.isPending && <Skeleton className="h-40 w-full" />}
          {data && (
            <>
              <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-2 text-sm">
                {facts.map((fact) => (
                  <div key={fact.label} className="contents">
                    <dt className="text-muted-foreground">{fact.label}</dt>
                    <dd className="tabular-nums">{fact.value}</dd>
                  </div>
                ))}
              </dl>
              {data.provider_responses.length > 1 && (
                <div className="space-y-2">
                  <p className="text-sm font-medium">
                    {t("organization:activity.record.attempts")}
                  </p>
                  <ul className="space-y-1 text-sm">
                    {data.provider_responses.map((attempt, index) => (
                      <li
                        key={`${attempt.created_at}-${index}`}
                        className="flex justify-between gap-4"
                      >
                        <span>
                          {attempt.provider}/{attempt.model}{" "}
                          <Badge
                            variant={
                              attempt.status === "error"
                                ? "destructive"
                                : "outline"
                            }
                          >
                            {attempt.status === "error"
                              ? t("organization:activity.record.error")
                              : t("organization:activity.record.success")}
                          </Badge>
                        </span>
                        <span className="tabular-nums">
                          {usd(attempt.cost)}
                        </span>
                      </li>
                    ))}
                  </ul>
                </div>
              )}
              <p className="text-muted-foreground text-xs">
                {t("organization:activity.record.note")}
              </p>
            </>
          )}
        </ModalBody>
      </ModalContent>
    </Modal>
  );
};
