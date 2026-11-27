"use client";

import { useQuery } from "@tanstack/react-query";
import { useState } from "react";

import { handle } from "@workspace/api/utils";
import { useTranslation } from "@workspace/i18n";
import { Badge } from "@workspace/ui-web/badge";
import { buttonVariants } from "@workspace/ui-web/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui-web/card";
import { Input } from "@workspace/ui-web/input";
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
import { api } from "~/lib/api/client";
import { useStaffPermission } from "~/modules/admin/lib/use-staff-permission";
import {
  STATUS_VARIANT,
  usePromoCodeFormat,
} from "~/modules/admin/promo-codes/format";
import { TurboLink } from "~/modules/common/turbo-link";

const STATUSES = [
  "active",
  "paused",
  "scheduled",
  "expired",
  "used_up",
] as const;
type Status = (typeof STATUSES)[number];
const ALL = "all";

/** Every promo code (BILL-08), searchable by its letters and filtered by status. */
export const PromoCodesList = () => {
  const { t } = useTranslation(["common", "admin"]);
  const format = usePromoCodeFormat();
  const canEdit = useStaffPermission({ promoCodes: ["update"] });
  const [search, setSearch] = useState("");
  const [status, setStatus] = useState<Status | typeof ALL>(ALL);

  const query = {
    ...(search.trim() ? { q: search.trim() } : {}),
    ...(status === ALL ? {} : { status }),
  };
  const codes = useQuery({
    queryKey: ["admin", "promo-codes", query],
    queryFn: () => handle(api.admin["promo-codes"].$get)({ query }),
  });

  const statuses = [
    { value: ALL, label: t("admin:promoCodes.list.allStatuses") },
    ...STATUSES.map((value) => ({
      value,
      label: t(`admin:promoCodes.status.${value}`),
    })),
  ];

  return (
    <Card>
      <CardHeader className="flex flex-wrap items-start justify-between gap-4">
        <div className="flex flex-col gap-1.5">
          <CardTitle>{t("admin:promoCodes.header.title")}</CardTitle>
          <CardDescription>
            {t("admin:promoCodes.header.description")}
          </CardDescription>
        </div>
        {canEdit && (
          <TurboLink
            href={pathsConfig.admin.promoCodes.new}
            className={buttonVariants()}
          >
            {t("admin:promoCodes.list.new")}
          </TurboLink>
        )}
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <div className="flex flex-wrap gap-2">
          <Input
            className="w-64"
            placeholder={t("admin:promoCodes.list.search")}
            aria-label={t("admin:promoCodes.list.search")}
            value={search}
            onChange={(event) => setSearch(event.target.value)}
          />
          <Select
            items={statuses}
            value={status}
            onValueChange={(value) =>
              setStatus(STATUSES.find((item) => item === value) ?? ALL)
            }
          >
            <SelectTrigger
              className="w-48"
              aria-label={t("admin:promoCodes.list.status")}
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
        </div>

        {codes.isPending && <Skeleton className="h-32 w-full" />}
        {codes.data?.length === 0 && (
          <p className="text-muted-foreground text-sm">
            {t("admin:promoCodes.list.empty")}
          </p>
        )}
        {codes.data && codes.data.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("admin:promoCodes.list.code")}</TableHead>
                <TableHead>{t("admin:promoCodes.list.kind")}</TableHead>
                <TableHead>{t("admin:promoCodes.list.value")}</TableHead>
                <TableHead>{t("admin:promoCodes.list.uses")}</TableHead>
                <TableHead>{t("admin:promoCodes.list.validity")}</TableHead>
                <TableHead>{t("admin:promoCodes.list.status")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {codes.data.map((code) => (
                <TableRow key={code.id}>
                  <TableCell className="font-mono font-medium">
                    <TurboLink
                      href={pathsConfig.admin.promoCodes.code(code.id)}
                      className="hover:underline"
                    >
                      {code.code}
                    </TurboLink>
                  </TableCell>
                  <TableCell>
                    {t(`admin:promoCodes.kind.${code.kind}`)}
                  </TableCell>
                  <TableCell>{format.value(code)}</TableCell>
                  <TableCell className="tabular-nums">
                    {format.uses(code)}
                  </TableCell>
                  <TableCell>{format.validity(code)}</TableCell>
                  <TableCell>
                    <Badge variant={STATUS_VARIANT[code.status]}>
                      {t(`admin:promoCodes.status.${code.status}`)}
                    </Badge>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
};
