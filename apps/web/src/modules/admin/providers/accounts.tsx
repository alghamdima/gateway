"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

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
import { Input } from "@workspace/ui-web/input";
import { Skeleton } from "@workspace/ui-web/skeleton";
import { toast } from "@workspace/ui-web/toast";

import { admin } from "~/modules/admin/lib/api";
import { useStaffPermission } from "~/modules/admin/lib/use-staff-permission";

interface ProviderKey {
  id: string;
  name: string;
  maskedValue: string;
  models: string[];
  isEnabled: boolean;
}

interface ProviderAccount {
  name: string;
  baseProvider: string | null;
  baseUrl: string | null;
  status: string;
  keys: ProviderKey[];
}

/**
 * The providers and their keys, which live in the gateway. Staff with the
 * update permission add and remove them here; a key's value is typed once
 * and never shown again.
 */
export const ProviderAccounts = () => {
  const { t } = useTranslation(["common", "admin"]);
  const accounts = useQuery(admin.queries.providers.getAccounts());
  const canEdit = useStaffPermission({ providers: ["update"] });

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("admin:providers.accounts.title")}</CardTitle>
        <CardDescription>
          {t("admin:providers.accounts.description")}
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-6">
        {accounts.isError && (
          <p className="text-destructive text-sm">
            {t("admin:providers.unavailable")}
          </p>
        )}
        {accounts.isPending && <Skeleton className="h-24 w-full" />}
        {accounts.data?.length === 0 && (
          <p className="text-muted-foreground text-sm">
            {t("admin:providers.accounts.empty")}
          </p>
        )}
        {accounts.data?.map((account) => (
          <Account key={account.name} account={account} canEdit={canEdit} />
        ))}
        {canEdit && <NewProvider />}
        {canEdit && <PriceSync />}
      </CardContent>
    </Card>
  );
};

/** After a change: read both lists again and say what happened. */
const useChanged = () => {
  const queryClient = useQueryClient();
  return async (title: string) => {
    await queryClient.invalidateQueries({ queryKey: ["admin", "providers"] });
    toast.add({ title, type: "success" });
  };
};

const onError = (error: Error) =>
  toast.add({ title: error.message, type: "error" });

/** The gateway reads its price list on a schedule; this reads it now. */
const PriceSync = () => {
  const { t } = useTranslation(["common", "admin"]);
  const changed = useChanged();
  const sync = useMutation({
    ...admin.mutations.providers.syncPrices,
    onSuccess: () => changed(t("admin:providers.accounts.pricesSynced")),
    onError,
  });

  return (
    <div className="flex flex-wrap items-center gap-3 border-t pt-4">
      <Button
        variant="outline"
        size="sm"
        type="button"
        disabled={sync.isPending}
        onClick={() => sync.mutate()}
      >
        {t("admin:providers.accounts.syncPrices")}
      </Button>
      <p className="text-muted-foreground text-xs">
        {t("admin:providers.accounts.syncPricesHint")}
      </p>
    </div>
  );
};

const Account = ({
  account,
  canEdit,
}: {
  account: ProviderAccount;
  canEdit: boolean;
}) => {
  const { t } = useTranslation(["common", "admin"]);
  const changed = useChanged();
  const provider = account.name;

  const refresh = useMutation({
    ...admin.mutations.providers.refreshModels,
    onSuccess: () => changed(t("admin:providers.accounts.refreshed")),
    onError,
  });
  const remove = useMutation({
    ...admin.mutations.providers.deleteAccount,
    onSuccess: () => changed(t("admin:providers.accounts.deleted")),
    onError,
  });

  return (
    <section
      className="flex flex-col gap-3 rounded-lg border p-4"
      data-provider={provider}
    >
      <header className="flex flex-wrap items-center gap-2">
        <h3 className="font-mono font-medium">{provider}</h3>
        {account.baseProvider && (
          <Badge variant="secondary">
            {t("admin:providers.accounts.custom", {
              base: account.baseProvider,
            })}
          </Badge>
        )}
        {account.baseUrl && (
          <span className="text-muted-foreground truncate text-xs">
            {account.baseUrl}
          </span>
        )}
        {canEdit && (
          <div className="ml-auto flex gap-2">
            <Button
              variant="outline"
              size="sm"
              type="button"
              disabled={refresh.isPending}
              onClick={() => refresh.mutate({ provider })}
            >
              {t("admin:providers.accounts.refresh")}
            </Button>
            <ConfirmButton
              label={t("admin:providers.accounts.delete")}
              isPending={remove.isPending}
              onConfirm={() => remove.mutate({ provider })}
            />
          </div>
        )}
      </header>

      {account.keys.length === 0 && (
        <p className="text-muted-foreground text-sm">
          {t("admin:providers.accounts.noKeys")}
        </p>
      )}
      <ul className="flex flex-col gap-2">
        {account.keys.map((key) => (
          <Key key={key.id} provider={provider} value={key} canEdit={canEdit} />
        ))}
      </ul>

      {canEdit && <NewKey provider={provider} />}
    </section>
  );
};

const Key = ({
  provider,
  value: key,
  canEdit,
}: {
  provider: string;
  value: ProviderKey;
  canEdit: boolean;
}) => {
  const { t } = useTranslation(["common", "admin"]);
  const changed = useChanged();

  const update = useMutation({
    ...admin.mutations.providers.updateKey,
    onSuccess: () => changed(t("admin:providers.accounts.keySaved")),
    onError,
  });
  const remove = useMutation({
    ...admin.mutations.providers.deleteKey,
    onSuccess: () => changed(t("admin:providers.accounts.keyDeleted")),
    onError,
  });

  return (
    <li
      className="flex flex-wrap items-center gap-2 text-sm"
      data-key={key.name}
    >
      <span className="font-medium">{key.name}</span>
      <span className="text-muted-foreground font-mono text-xs">
        {key.maskedValue}
      </span>
      <Badge variant={key.isEnabled ? "secondary" : "outline"}>
        {key.isEnabled
          ? t("admin:providers.accounts.enabled")
          : t("admin:providers.accounts.disabled")}
      </Badge>
      <span className="text-muted-foreground text-xs">
        {key.models.join(", ")}
      </span>
      {canEdit && (
        <div className="ml-auto flex gap-2">
          <Button
            variant="ghost"
            size="sm"
            type="button"
            disabled={update.isPending}
            onClick={() =>
              update.mutate({
                provider,
                keyId: key.id,
                isEnabled: !key.isEnabled,
              })
            }
          >
            {key.isEnabled
              ? t("admin:providers.accounts.disable")
              : t("admin:providers.accounts.enable")}
          </Button>
          <ConfirmButton
            label={t("admin:providers.accounts.delete")}
            isPending={remove.isPending}
            onConfirm={() => remove.mutate({ provider, keyId: key.id })}
          />
        </div>
      )}
    </li>
  );
};

const NewKey = ({ provider }: { provider: string }) => {
  const { t } = useTranslation(["common", "admin"]);
  const changed = useChanged();
  const [name, setName] = useState("");
  const [value, setValue] = useState("");

  const create = useMutation({
    ...admin.mutations.providers.createKey,
    onSuccess: async () => {
      setName("");
      setValue("");
      await changed(t("admin:providers.accounts.keySaved"));
    },
    onError,
  });

  return (
    <form
      className="flex flex-wrap items-end gap-2"
      onSubmit={(event) => {
        event.preventDefault();
        create.mutate({ provider, name: name.trim(), value });
      }}
    >
      <Input
        className="w-40"
        aria-label={t("admin:providers.accounts.keyName")}
        placeholder={t("admin:providers.accounts.keyName")}
        value={name}
        onChange={(event) => setName(event.target.value)}
        required
      />
      <Input
        className="w-72"
        type="password"
        autoComplete="off"
        aria-label={t("admin:providers.accounts.keyValue")}
        placeholder={t("admin:providers.accounts.keyValue")}
        value={value}
        onChange={(event) => setValue(event.target.value)}
        required
      />
      <Button type="submit" size="sm" disabled={create.isPending}>
        {t("admin:providers.accounts.addKey")}
      </Button>
    </form>
  );
};

const NewProvider = () => {
  const { t } = useTranslation(["common", "admin"]);
  const changed = useChanged();
  const [name, setName] = useState("");
  const [baseUrl, setBaseUrl] = useState("");

  const create = useMutation({
    ...admin.mutations.providers.createAccount,
    onSuccess: async () => {
      setName("");
      setBaseUrl("");
      await changed(t("admin:providers.accounts.created"));
    },
    onError,
  });

  return (
    <form
      className="flex flex-col gap-2 border-t pt-4"
      onSubmit={(event) => {
        event.preventDefault();
        const url = baseUrl.trim();
        create.mutate({
          name: name.trim(),
          // an address makes it a provider of our own that speaks the OpenAI protocol
          ...(url ? { baseProvider: "openai", baseUrl: url } : {}),
        });
      }}
    >
      <p className="text-sm font-medium">{t("admin:providers.accounts.add")}</p>
      <div className="flex flex-wrap items-end gap-2">
        <Input
          className="w-48"
          aria-label={t("admin:providers.accounts.name")}
          placeholder={t("admin:providers.accounts.name")}
          value={name}
          onChange={(event) => setName(event.target.value)}
          required
        />
        <Input
          className="w-80"
          type="url"
          aria-label={t("admin:providers.accounts.baseUrl")}
          placeholder={t("admin:providers.accounts.baseUrl")}
          value={baseUrl}
          onChange={(event) => setBaseUrl(event.target.value)}
        />
        <Button type="submit" size="sm" disabled={create.isPending}>
          {t("admin:providers.accounts.addProvider")}
        </Button>
      </div>
      <p className="text-muted-foreground text-xs">
        {t("admin:providers.accounts.addHint")}
      </p>
    </form>
  );
};

/** Deleting takes two clicks: the second one, on the same button, confirms. */
const ConfirmButton = ({
  label,
  isPending,
  onConfirm,
}: {
  label: string;
  isPending: boolean;
  onConfirm: () => void;
}) => {
  const { t } = useTranslation(["common", "admin"]);
  const [isArmed, setIsArmed] = useState(false);

  return (
    <Button
      variant={isArmed ? "destructive" : "ghost"}
      size="sm"
      type="button"
      disabled={isPending}
      onBlur={() => setIsArmed(false)}
      onClick={() => {
        if (!isArmed) {
          setIsArmed(true);
          return;
        }
        setIsArmed(false);
        onConfirm();
      }}
    >
      {isArmed ? t("admin:providers.accounts.confirm") : label}
    </Button>
  );
};
