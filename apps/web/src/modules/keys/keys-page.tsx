"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { toMemberRole } from "@workspace/auth";
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
import { Icons } from "@workspace/ui-web/icons";
import { Input } from "@workspace/ui-web/input";
import {
  Modal,
  ModalBody,
  ModalClose,
  ModalContent,
  ModalDescription,
  ModalFooter,
  ModalHeader,
  ModalTitle,
} from "@workspace/ui-web/modal";
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

import { authClient } from "~/lib/auth/client";
import { useCopy } from "~/modules/common/hooks/use-copy";
import { KeyFormModal, toFormValues } from "~/modules/keys/key-form";
import { keys } from "~/modules/keys/lib/api";
import { formatMicroSar } from "~/modules/keys/money";
import { useActiveOrganization } from "~/modules/organization/hooks/use-active-organization";

import type { KeyFormResult } from "~/modules/keys/key-form";

interface KeysPageProps {
  readonly organizationId: string;
}

interface ApiKeyRow {
  id: string;
  name: string;
  prefix: string;
  limitMicroSar: number | null;
  limitReset: string | null;
  expiresAt: string | null;
  disabledAt: string | null;
  createdAt: string;
}

export const KeysPage = ({ organizationId }: KeysPageProps) => {
  const { t, i18n } = useTranslation(["common", "organization"]);
  const queryClient = useQueryClient();
  const { activeMember } = useActiveOrganization();
  const list = useQuery(keys.queries.list(organizationId));

  // members see keys; owners and admins manage them (ADR 0004). The API checks the same.
  const canManage = authClient.organization.checkRolePermission({
    permissions: { apiKey: ["create"] },
    role: toMemberRole(activeMember?.role),
  });

  const [isCreating, setIsCreating] = useState(false);
  const [editing, setEditing] = useState<ApiKeyRow | null>(null);
  const [deleting, setDeleting] = useState<ApiKeyRow | null>(null);
  const [secret, setSecret] = useState<string | null>(null);

  const refresh = () =>
    queryClient.invalidateQueries(keys.queries.list(organizationId));

  const create = useMutation({ ...keys.mutations.create, onSuccess: refresh });
  const update = useMutation({ ...keys.mutations.update, onSuccess: refresh });
  const remove = useMutation({
    ...keys.mutations.delete,
    onSuccess: async () => {
      await refresh();
      setDeleting(null);
      toast.add({
        title: t("organization:keys.delete.success"),
        type: "success",
      });
    },
  });

  const handleCreate = async (result: KeyFormResult) => {
    const created = await create.mutateAsync({ organizationId, ...result });
    setIsCreating(false);
    setSecret(created.key);
  };

  const handleEdit = async (result: KeyFormResult) => {
    if (!editing) return;
    await update.mutateAsync({ organizationId, keyId: editing.id, ...result });
    setEditing(null);
    toast.add({ title: t("organization:keys.edit.success"), type: "success" });
  };

  const describeLimit = (row: ApiKeyRow) =>
    row.limitMicroSar === null
      ? t("organization:keys.list.noLimit")
      : `${formatMicroSar(row.limitMicroSar, i18n.language)} · ${t(resetLabelKey(row.limitReset))}`;

  const rows: ApiKeyRow[] = list.data ?? [];

  return (
    <Card className="w-full">
      <CardHeader className="flex flex-row items-start justify-between gap-4">
        <div className="space-y-1.5">
          <CardTitle>{t("organization:keys.title")}</CardTitle>
          <CardDescription>
            {t("organization:keys.description")}
          </CardDescription>
        </div>
        {canManage && (
          <Button onClick={() => setIsCreating(true)}>
            <Icons.Plus className="size-4" />
            {t("organization:keys.create.cta")}
          </Button>
        )}
      </CardHeader>
      <CardContent>
        {list.isPending && <Skeleton className="h-32 w-full" />}
        {!list.isPending && rows.length === 0 && (
          <p className="text-muted-foreground text-sm">
            {t("organization:keys.list.empty")}
          </p>
        )}
        {rows.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("organization:keys.list.name")}</TableHead>
                <TableHead>{t("organization:keys.list.key")}</TableHead>
                <TableHead>{t("organization:keys.list.limit")}</TableHead>
                <TableHead>{t("organization:keys.list.status")}</TableHead>
                <TableHead>{t("organization:keys.list.created")}</TableHead>
                {canManage && <TableHead className="w-0" />}
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((row) => (
                <TableRow key={row.id}>
                  <TableCell>{row.name}</TableCell>
                  <TableCell className="font-mono">{row.prefix}…</TableCell>
                  <TableCell>{describeLimit(row)}</TableCell>
                  <TableCell>
                    {row.disabledAt ? (
                      <Badge variant="outline">
                        {t("organization:keys.list.disabled")}
                      </Badge>
                    ) : (
                      <Badge variant="success">
                        {t("organization:keys.list.active")}
                      </Badge>
                    )}
                  </TableCell>
                  <TableCell className="text-muted-foreground whitespace-nowrap">
                    {new Date(row.createdAt).toLocaleDateString(i18n.language)}
                  </TableCell>
                  {canManage && (
                    <TableCell className="flex gap-1">
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() => setEditing(row)}
                      >
                        {t("edit")}
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        disabled={update.isPending}
                        onClick={() =>
                          update.mutate({
                            organizationId,
                            keyId: row.id,
                            disabled: !row.disabledAt,
                          })
                        }
                      >
                        {row.disabledAt
                          ? t("organization:keys.list.enable")
                          : t("organization:keys.list.disable")}
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon"
                        aria-label={t("common:delete")}
                        onClick={() => setDeleting(row)}
                      >
                        <Icons.Trash className="size-4" />
                      </Button>
                    </TableCell>
                  )}
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>

      <KeyFormModal
        open={isCreating}
        onOpenChange={setIsCreating}
        title={t("organization:keys.create.title")}
        submitLabel={t("organization:keys.create.cta")}
        onSubmit={handleCreate}
      />
      <KeyFormModal
        open={editing !== null}
        onOpenChange={(open) => !open && setEditing(null)}
        title={t("organization:keys.edit.title")}
        submitLabel={t("save")}
        initial={editing ? toFormValues(editing) : undefined}
        onSubmit={handleEdit}
      />
      <SecretModal secret={secret} onClose={() => setSecret(null)} />
      <Modal
        open={deleting !== null}
        drawer={{ onOpenChange: (open) => !open && setDeleting(null) }}
        dialog={{ onOpenChange: (open) => !open && setDeleting(null) }}
      >
        <ModalContent>
          <ModalHeader>
            <ModalTitle>
              {t("organization:keys.delete.title", {
                name: deleting?.name ?? "",
              })}
            </ModalTitle>
            <ModalDescription>
              {t("organization:keys.delete.description")}
            </ModalDescription>
          </ModalHeader>
          <ModalFooter>
            <ModalClose
              render={
                <Button variant="outline" type="button">
                  {t("cancel")}
                </Button>
              }
            />
            <Button
              variant="destructive"
              disabled={remove.isPending}
              onClick={() =>
                deleting &&
                remove.mutate({ organizationId, keyId: deleting.id })
              }
            >
              {t("common:delete")}
            </Button>
          </ModalFooter>
        </ModalContent>
      </Modal>
    </Card>
  );
};

const resetLabelKey = (reset: string | null) => {
  switch (reset) {
    case "daily":
      return "organization:keys.form.reset.daily";
    case "weekly":
      return "organization:keys.form.reset.weekly";
    case "monthly":
      return "organization:keys.form.reset.monthly";
    default:
      return "organization:keys.form.reset.never";
  }
};

/** The new key, shown once: it is not stored anywhere it could be shown again. */
const SecretModal = ({
  secret,
  onClose,
}: {
  readonly secret: string | null;
  readonly onClose: () => void;
}) => {
  const { t } = useTranslation(["common", "organization"]);
  const { copied, copy } = useCopy();

  return (
    <Modal
      open={secret !== null}
      drawer={{ onOpenChange: (open) => !open && onClose() }}
      dialog={{ onOpenChange: (open) => !open && onClose() }}
    >
      <ModalContent>
        <ModalHeader>
          <ModalTitle>{t("organization:keys.secret.title")}</ModalTitle>
          <ModalDescription>
            {t("organization:keys.secret.description")}
          </ModalDescription>
        </ModalHeader>
        <ModalBody>
          <div className="flex items-center gap-2">
            <Input
              readOnly
              value={secret ?? ""}
              className="font-mono"
              aria-label={t("organization:keys.list.key")}
              onFocus={(event) => event.target.select()}
            />
            <Button
              variant="outline"
              size="icon"
              aria-label={t("organization:keys.secret.copy")}
              onClick={() => secret && void copy(secret)}
            >
              {copied ? (
                <Icons.Check className="size-4" />
              ) : (
                <Icons.Copy className="size-4" />
              )}
            </Button>
          </div>
        </ModalBody>
        <ModalFooter>
          <Button onClick={onClose}>
            {t("organization:keys.secret.done")}
          </Button>
        </ModalFooter>
      </ModalContent>
    </Modal>
  );
};
