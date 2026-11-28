"use client";

import { useQuery } from "@tanstack/react-query";
import { useState } from "react";

import { CONTACT_STATUSES, CONTACT_TOPICS } from "@workspace/api/schema";
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
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@workspace/ui-web/select";
import { Skeleton } from "@workspace/ui-web/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@workspace/ui-web/table";

import { pathsConfig } from "~/config/paths";
import { admin } from "~/modules/admin/lib/api";
import { TurboLink } from "~/modules/common/turbo-link";

type Status = (typeof CONTACT_STATUSES)[number];
type Topic = (typeof CONTACT_TOPICS)[number];
const ALL = "all";

export const STATUS_VARIANT = {
  new: "default",
  in_progress: "secondary",
  closed: "outline",
} as const satisfies Record<Status, "default" | "secondary" | "outline">;

/** The messages sent through the contact page (SITE-03), newest first. */
export const ContactRequestsList = () => {
  const { t, i18n } = useTranslation(["common", "admin"]);
  const [status, setStatus] = useState<Status | typeof ALL>(ALL);
  const [topic, setTopic] = useState<Topic | typeof ALL>(ALL);
  const [page, setPage] = useState(1);

  const requests = useQuery(
    admin.queries.contactRequests.list({
      ...(status === ALL ? {} : { status }),
      ...(topic === ALL ? {} : { topic }),
      page: String(page),
    }),
  );
  const pages = requests.data
    ? Math.max(1, Math.ceil(requests.data.total / requests.data.pageSize))
    : 1;

  const statuses = [
    { value: ALL, label: t("admin:contactRequests.list.allStatuses") },
    ...CONTACT_STATUSES.map((value) => ({
      value,
      label: t(`admin:contactRequests.status.${value}`),
    })),
  ];
  const topics = [
    { value: ALL, label: t("admin:contactRequests.list.allTopics") },
    ...CONTACT_TOPICS.map((value) => ({
      value,
      label: t(`admin:contactRequests.topic.${value}`),
    })),
  ];

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("admin:contactRequests.header.title")}</CardTitle>
        <CardDescription>
          {t("admin:contactRequests.header.description")}
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        {requests.data && (
          <p className="text-sm font-medium">
            {requests.data.waiting === 0
              ? t("admin:contactRequests.waitingNone")
              : t("admin:contactRequests.waiting", {
                  count: requests.data.waiting,
                })}
          </p>
        )}
        <div className="flex flex-wrap gap-2">
          <Select
            items={statuses}
            value={status}
            onValueChange={(value) => {
              setStatus(CONTACT_STATUSES.find((item) => item === value) ?? ALL);
              setPage(1);
            }}
          >
            <SelectTrigger
              className="w-48"
              aria-label={t("admin:contactRequests.list.status")}
            >
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {statuses.map((item) => (
                <SelectItem key={item.value} value={item.value}>
                  {item.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Select
            items={topics}
            value={topic}
            onValueChange={(value) => {
              setTopic(CONTACT_TOPICS.find((item) => item === value) ?? ALL);
              setPage(1);
            }}
          >
            <SelectTrigger
              className="w-48"
              aria-label={t("admin:contactRequests.list.topic")}
            >
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {topics.map((item) => (
                <SelectItem key={item.value} value={item.value}>
                  {item.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>

        {requests.isPending && <Skeleton className="h-32 w-full" />}
        {requests.data?.items.length === 0 && (
          <p className="text-muted-foreground text-sm">
            {t("admin:contactRequests.list.empty")}
          </p>
        )}
        {requests.data && requests.data.items.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("admin:contactRequests.list.from")}</TableHead>
                <TableHead>{t("admin:contactRequests.list.company")}</TableHead>
                <TableHead>{t("admin:contactRequests.list.topic")}</TableHead>
                <TableHead>
                  {t("admin:contactRequests.list.received")}
                </TableHead>
                <TableHead>{t("admin:contactRequests.list.status")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {requests.data.items.map((request) => (
                <TableRow key={request.id}>
                  <TableCell>
                    <TurboLink
                      href={pathsConfig.admin.contactRequests.request(
                        request.id,
                      )}
                      className="flex flex-col hover:underline"
                    >
                      <span className="font-medium">{request.name}</span>
                      <span className="text-muted-foreground text-sm">
                        {request.email}
                      </span>
                    </TurboLink>
                  </TableCell>
                  <TableCell>{request.company ?? "—"}</TableCell>
                  <TableCell>
                    {t(`admin:contactRequests.topic.${request.topic}`)}
                  </TableCell>
                  <TableCell className="text-muted-foreground whitespace-nowrap">
                    {new Date(request.createdAt).toLocaleString(i18n.language)}
                  </TableCell>
                  <TableCell>
                    <Badge variant={STATUS_VARIANT[request.status]}>
                      {t(`admin:contactRequests.status.${request.status}`)}
                    </Badge>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        {requests.data && pages > 1 && (
          <div className="flex items-center justify-between gap-4">
            <span className="text-muted-foreground text-sm">
              {t("admin:contactRequests.list.page", { page, pages })}
            </span>
            <div className="flex gap-2">
              <Button
                variant="outline"
                size="sm"
                disabled={page <= 1}
                onClick={() => setPage(page - 1)}
              >
                {t("admin:contactRequests.list.previous")}
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={page >= pages}
                onClick={() => setPage(page + 1)}
              >
                {t("admin:contactRequests.list.next")}
              </Button>
            </div>
          </div>
        )}
      </CardContent>
    </Card>
  );
};
