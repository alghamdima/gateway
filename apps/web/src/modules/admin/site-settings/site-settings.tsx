"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import * as z from "zod";

import { useTranslation } from "@workspace/i18n";
import { Button } from "@workspace/ui-web/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui-web/card";
import { Input } from "@workspace/ui-web/input";
import { Label } from "@workspace/ui-web/label";
import { Skeleton } from "@workspace/ui-web/skeleton";
import { Switch } from "@workspace/ui-web/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@workspace/ui-web/table";
import { toast } from "@workspace/ui-web/toast";

import { admin } from "~/modules/admin/lib/api";
import { useStaffPermission } from "~/modules/admin/lib/use-staff-permission";

const emailSchema = z.email().max(254);

/**
 * Which parts of the public site are switched on (SITE-01) and where contact
 * messages are announced (ADMIN-20), with the history.
 */
export const SiteSettings = () => {
  const { t, i18n } = useTranslation(["common", "admin"]);
  const queryClient = useQueryClient();
  const settings = useQuery(admin.queries.siteSettings.get());
  const changes = useQuery(admin.queries.siteSettings.getChanges());
  const canEdit = useStaffPermission({ site: ["update"] });
  // what staff typed; until they type, the stored address shows
  const [emailDraft, setEmailDraft] = useState<string | null>(null);

  // every save sends both values: the one changed over the stored other
  const current = {
    isBlogEnabled: settings.data?.isBlogEnabled ?? false,
    contactEmail: settings.data?.contactEmail ?? null,
  };
  const email = emailDraft ?? current.contactEmail ?? "";
  const isEmailValid =
    email.trim() === "" || emailSchema.safeParse(email.trim()).success;

  const save = useMutation({
    ...admin.mutations.siteSettings.update,
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({
          queryKey: admin.queries.siteSettings.get().queryKey,
        }),
        queryClient.invalidateQueries({
          queryKey: admin.queries.siteSettings.getChanges().queryKey,
        }),
      ]);
      setEmailDraft(null);
      toast.add({ title: t("admin:siteSettings.saved"), type: "success" });
    },
  });

  const describe = (values: Record<string, unknown>) =>
    [
      typeof values.isBlogEnabled === "boolean"
        ? `${t("admin:siteSettings.changes.fields.isBlogEnabled")}: ${t(values.isBlogEnabled ? "admin:siteSettings.changes.on" : "admin:siteSettings.changes.off")}`
        : null,
      "contactEmail" in values
        ? `${t("admin:siteSettings.changes.fields.contactEmail")}: ${typeof values.contactEmail === "string" ? values.contactEmail : t("admin:siteSettings.changes.none")}`
        : null,
    ]
      .filter(Boolean)
      .join(", ");

  return (
    <div className="@container flex w-full flex-col gap-6">
      <Card>
        <CardHeader>
          <CardTitle>{t("admin:siteSettings.blog.title")}</CardTitle>
          <CardDescription>
            {t("admin:siteSettings.blog.description")}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {settings.data ? (
            <div className="flex items-center gap-3">
              <Switch
                id="admin-site-blog-enabled"
                checked={settings.data.isBlogEnabled}
                disabled={!canEdit || save.isPending}
                onCheckedChange={(checked) =>
                  save.mutate({ ...current, isBlogEnabled: checked })
                }
              />
              <Label htmlFor="admin-site-blog-enabled">
                {t("admin:siteSettings.blog.label")}
              </Label>
            </div>
          ) : (
            <Skeleton className="h-8 w-full" />
          )}
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>{t("admin:siteSettings.contactEmail.title")}</CardTitle>
          <CardDescription>
            {t("admin:siteSettings.contactEmail.description")}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {settings.data ? (
            <form
              className="flex max-w-md flex-col gap-2"
              onSubmit={(event) => {
                event.preventDefault();
                if (isEmailValid) {
                  save.mutate({
                    ...current,
                    contactEmail: email.trim() || null,
                  });
                }
              }}
            >
              <Label htmlFor="admin-site-contact-email">
                {t("admin:siteSettings.contactEmail.label")}
              </Label>
              <div className="flex gap-2">
                <Input
                  id="admin-site-contact-email"
                  type="email"
                  inputMode="email"
                  spellCheck={false}
                  autoComplete="off"
                  value={email}
                  disabled={!canEdit}
                  aria-invalid={!isEmailValid}
                  placeholder={t("admin:siteSettings.contactEmail.placeholder")}
                  onChange={(event) => setEmailDraft(event.target.value)}
                />
                {canEdit && (
                  <Button
                    type="submit"
                    disabled={
                      save.isPending || emailDraft === null || !isEmailValid
                    }
                  >
                    {t("admin:siteSettings.contactEmail.save")}
                  </Button>
                )}
              </div>
              {!isEmailValid && (
                <p className="text-destructive text-sm">
                  {t("admin:siteSettings.contactEmail.invalid")}
                </p>
              )}
            </form>
          ) : (
            <Skeleton className="h-16 w-full" />
          )}
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>{t("admin:siteSettings.changes.title")}</CardTitle>
          <CardDescription>
            {t("admin:siteSettings.changes.description")}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {!changes.data && <Skeleton className="h-24 w-full" />}
          {changes.data?.length === 0 && (
            <p className="text-muted-foreground text-sm">
              {t("admin:siteSettings.changes.empty")}
            </p>
          )}
          {changes.data && changes.data.length > 0 && (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t("admin:keySettings.changes.when")}</TableHead>
                  <TableHead>{t("admin:keySettings.changes.who")}</TableHead>
                  <TableHead>{t("admin:keySettings.changes.before")}</TableHead>
                  <TableHead>{t("admin:keySettings.changes.after")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {changes.data.map((change) => (
                  <TableRow key={change.id}>
                    <TableCell className="text-muted-foreground whitespace-nowrap">
                      {new Date(change.changedAt).toLocaleString(i18n.language)}
                    </TableCell>
                    <TableCell>
                      {change.changedByName ?? change.changedByEmail ?? "—"}
                    </TableCell>
                    <TableCell className="text-sm whitespace-normal">
                      {describe(change.before)}
                    </TableCell>
                    <TableCell className="text-sm whitespace-normal">
                      {describe(change.after)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </div>
  );
};
