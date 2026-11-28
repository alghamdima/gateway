"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { CONTACT_NOTE_MAX, CONTACT_STATUSES } from "@workspace/api/schema";
import { useTranslation } from "@workspace/i18n";
import { Badge } from "@workspace/ui-web/badge";
import { Button, buttonVariants } from "@workspace/ui-web/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui-web/card";
import { Label } from "@workspace/ui-web/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@workspace/ui-web/select";
import { Skeleton } from "@workspace/ui-web/skeleton";
import { Textarea } from "@workspace/ui-web/textarea";
import { toast } from "@workspace/ui-web/toast";

import { appConfig } from "~/config/app";
import { pathsConfig } from "~/config/paths";
import { STATUS_VARIANT } from "~/modules/admin/contact-requests/contact-requests-list";
import { admin } from "~/modules/admin/lib/api";
import { useStaffPermission } from "~/modules/admin/lib/use-staff-permission";
import { TurboLink } from "~/modules/common/turbo-link";

type Status = (typeof CONTACT_STATUSES)[number];

/**
 * One contact message (SITE-03): what was sent and by whom, a link that
 * opens a reply in the staff member's own mail, and where it stands.
 */
export const ContactRequestDetail = ({ id }: { id: string }) => {
  const { t, i18n } = useTranslation(["common", "admin"]);
  const queryClient = useQueryClient();
  const request = useQuery(admin.queries.contactRequests.get(id));
  const canEdit = useStaffPermission({ contact: ["update"] });
  // what staff typed; until they type, the stored values show
  const [draft, setDraft] = useState<{
    status?: Status;
    staffNote?: string;
  }>({});

  const save = useMutation({
    ...admin.mutations.contactRequests.update,
    onSuccess: async () => {
      await queryClient.invalidateQueries({
        queryKey: ["admin", "contact-requests"],
      });
      setDraft({});
      toast.add({
        title: t("admin:contactRequests.detail.handling.saved"),
        type: "success",
      });
    },
  });

  if (!request.data) {
    return <Skeleton className="h-64 w-full" />;
  }
  const data = request.data;
  const status = draft.status ?? data.status;
  const staffNote = draft.staffNote ?? data.staffNote ?? "";
  const statuses = CONTACT_STATUSES.map((value) => ({
    value,
    label: t(`admin:contactRequests.status.${value}`),
  }));
  const replyHref = `mailto:${data.email}?subject=${encodeURIComponent(
    t("admin:contactRequests.detail.replySubject", { product: appConfig.name }),
  )}`;

  return (
    <div className="@container flex w-full flex-col gap-6">
      <TurboLink
        href={pathsConfig.admin.contactRequests.index}
        className="text-muted-foreground text-sm hover:underline"
      >
        ← {t("admin:contactRequests.detail.back")}
      </TurboLink>
      <Card>
        <CardHeader className="flex flex-wrap items-start justify-between gap-4">
          <div className="flex flex-col gap-1.5">
            <CardTitle className="flex flex-wrap items-center gap-2">
              {data.name}
              <Badge variant="outline">
                {t(`admin:contactRequests.topic.${data.topic}`)}
              </Badge>
              <Badge variant={STATUS_VARIANT[data.status]}>
                {t(`admin:contactRequests.status.${data.status}`)}
              </Badge>
            </CardTitle>
            <CardDescription>
              {t("admin:contactRequests.detail.received")}:{" "}
              {new Date(data.createdAt).toLocaleString(i18n.language)}
            </CardDescription>
          </div>
          <a href={replyHref} className={buttonVariants()}>
            {t("admin:contactRequests.detail.reply")}
          </a>
        </CardHeader>
        <CardContent className="flex flex-col gap-6">
          <dl className="grid gap-4 text-sm @lg:grid-cols-3">
            <div className="flex flex-col gap-1">
              <dt className="text-muted-foreground">
                {t("admin:contactRequests.detail.email")}
              </dt>
              <dd className="font-medium break-all">{data.email}</dd>
            </div>
            <div className="flex flex-col gap-1">
              <dt className="text-muted-foreground">
                {t("admin:contactRequests.detail.company")}
              </dt>
              <dd className="font-medium">{data.company ?? "—"}</dd>
            </div>
            <div className="flex flex-col gap-1">
              <dt className="text-muted-foreground">
                {t("admin:contactRequests.detail.account")}
              </dt>
              <dd className="font-medium">
                {data.userId ? (
                  <TurboLink
                    href={pathsConfig.admin.users.user(data.userId)}
                    className="hover:underline"
                  >
                    {t("admin:contactRequests.detail.accountLink")}
                  </TurboLink>
                ) : (
                  t("admin:contactRequests.detail.noAccount")
                )}
              </dd>
            </div>
          </dl>
          <div className="flex flex-col gap-2">
            <h3 className="text-sm font-medium">
              {t("admin:contactRequests.detail.message")}
            </h3>
            <p className="bg-muted/50 rounded-md p-4 text-sm whitespace-pre-wrap">
              {data.message}
            </p>
          </div>
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>
            {t("admin:contactRequests.detail.handling.title")}
          </CardTitle>
          <CardDescription>
            {t("admin:contactRequests.detail.handling.description")}
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <div className="flex flex-col gap-2">
            <Label htmlFor="admin-contact-status">
              {t("admin:contactRequests.detail.handling.status")}
            </Label>
            <Select
              items={statuses}
              value={status}
              disabled={!canEdit}
              onValueChange={(value) =>
                setDraft({
                  ...draft,
                  status:
                    CONTACT_STATUSES.find((item) => item === value) ?? status,
                })
              }
            >
              <SelectTrigger id="admin-contact-status" className="w-48">
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
          <div className="flex flex-col gap-2">
            <Label htmlFor="admin-contact-note">
              {t("admin:contactRequests.detail.handling.note")}
            </Label>
            <Textarea
              id="admin-contact-note"
              value={staffNote}
              disabled={!canEdit}
              maxLength={CONTACT_NOTE_MAX}
              placeholder={t(
                "admin:contactRequests.detail.handling.notePlaceholder",
              )}
              onChange={(event) =>
                setDraft({ ...draft, staffNote: event.target.value })
              }
            />
          </div>
          {data.updatedBy && (
            <p className="text-muted-foreground text-sm">
              {t("admin:contactRequests.detail.handling.lastChange", {
                when: new Date(data.updatedAt).toLocaleString(i18n.language),
                who: data.updatedByName ?? data.updatedBy,
              })}
            </p>
          )}
          {canEdit && (
            <Button
              className="self-start"
              disabled={save.isPending}
              onClick={() =>
                save.mutate({ id, status, staffNote: staffNote || null })
              }
            >
              {t("admin:contactRequests.detail.handling.save")}
            </Button>
          )}
        </CardContent>
      </Card>
    </div>
  );
};
