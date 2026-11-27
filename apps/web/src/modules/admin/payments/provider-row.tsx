"use client";

import { useState } from "react";

import { useTranslation } from "@workspace/i18n";
import { cn } from "@workspace/ui";
import { Badge } from "@workspace/ui-web/badge";
import { Button } from "@workspace/ui-web/button";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@workspace/ui-web/dialog";
import { Icons } from "@workspace/ui-web/icons";
import { Input } from "@workspace/ui-web/input";
import { Label } from "@workspace/ui-web/label";
import { RadioGroupItem } from "@workspace/ui-web/radio-group";
import { Switch } from "@workspace/ui-web/switch";
import { toast } from "@workspace/ui-web/toast";

import type { PaymentProviderView } from "~/modules/admin/payments/types";

type Mode = PaymentProviderView["mode"];

const MODES: readonly Mode[] = ["test", "live"];

interface ProviderRowProps {
  readonly provider: PaymentProviderView;
  readonly canEdit: boolean;
  readonly isSaving: boolean;
  readonly onChange: (values: { enabled: boolean; mode: Mode }) => void;
}

/** One payment provider: active, enabled, mode, which secrets are set, its webhook URL. */
export const ProviderRow = ({
  provider,
  canEdit,
  isSaving,
  onChange,
}: ProviderRowProps) => {
  const { t } = useTranslation(["common", "admin"]);
  const [isConfirmingLive, setIsConfirmingLive] = useState(false);
  const isDisabled = !canEdit || isSaving;
  const id = `admin-payments-${provider.provider}`;

  const chooseMode = (mode: Mode) => {
    if (mode === provider.mode) {
      return;
    }
    // live charges real cards: staff confirm it first
    if (mode === "live") {
      setIsConfirmingLive(true);
      return;
    }
    onChange({ enabled: provider.enabled, mode });
  };

  const copyWebhookUrl = async () => {
    await navigator.clipboard.writeText(provider.webhookUrl);
    toast.add({
      title: t("admin:payments.providers.copied"),
      type: "success",
    });
  };

  return (
    <div className="space-y-4 rounded-lg border p-4">
      <div className="flex flex-wrap items-center justify-between gap-4">
        <div className="flex items-center gap-3">
          <RadioGroupItem
            id={`${id}-active`}
            value={provider.provider}
            disabled={isDisabled || !provider.enabled}
            aria-label={t("admin:payments.providers.makeActive")}
          />
          <Label htmlFor={`${id}-active`} className="text-base font-semibold">
            {t(`admin:payments.providers.names.${provider.provider}`)}
          </Label>
          {provider.isActive && (
            <Badge>{t("admin:payments.providers.active")}</Badge>
          )}
          <Badge variant={provider.mode === "live" ? "default" : "outline"}>
            {t(`admin:payments.mode.${provider.mode}`)}
          </Badge>
        </div>
        <div className="flex flex-wrap items-center gap-6">
          <div className="flex items-center gap-2">
            <Switch
              id={`${id}-enabled`}
              checked={provider.enabled}
              disabled={isDisabled}
              onCheckedChange={(checked) =>
                onChange({ enabled: checked, mode: provider.mode })
              }
            />
            <Label htmlFor={`${id}-enabled`}>
              {t("admin:payments.providers.enabled")}
            </Label>
          </div>
          <div
            role="group"
            aria-label={t("admin:payments.providers.mode")}
            className="flex gap-1"
          >
            {MODES.map((mode) => (
              <Button
                key={mode}
                type="button"
                size="sm"
                variant={provider.mode === mode ? "default" : "outline"}
                disabled={isDisabled}
                onClick={() => chooseMode(mode)}
              >
                {t(`admin:payments.mode.${mode}`)}
              </Button>
            ))}
          </div>
        </div>
      </div>

      {provider.secrets && (
        <div className="grid gap-4 @lg:grid-cols-2">
          {MODES.map((mode) => (
            <SecretsOfMode
              key={mode}
              mode={mode}
              secrets={provider.secrets?.[mode]}
              isCurrent={provider.mode === mode}
            />
          ))}
        </div>
      )}

      <div className="space-y-1">
        <Label htmlFor={`${id}-webhook`}>
          {t("admin:payments.providers.webhookUrl")}
        </Label>
        <div className="flex max-w-xl items-center gap-2">
          <Input
            id={`${id}-webhook`}
            value={provider.webhookUrl}
            readOnly
            className="font-mono"
          />
          <Button
            type="button"
            variant="outline"
            size="icon"
            onClick={copyWebhookUrl}
            aria-label={t("admin:payments.providers.copy")}
          >
            <Icons.Copy className="size-4" />
          </Button>
        </div>
        <p className="text-muted-foreground text-sm">
          {t("admin:payments.providers.webhookUrlDescription")}
        </p>
      </div>

      <Dialog open={isConfirmingLive} onOpenChange={setIsConfirmingLive}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("admin:payments.live.title")}</DialogTitle>
            <DialogDescription>
              {t("admin:payments.live.description")}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <DialogClose
              render={<Button variant="outline">{t("cancel")}</Button>}
            />
            <Button
              variant="destructive"
              onClick={() => {
                setIsConfirmingLive(false);
                onChange({ enabled: provider.enabled, mode: "live" });
              }}
            >
              {t("admin:payments.live.confirm")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
};

interface SecretsOfModeProps {
  readonly mode: Mode;
  readonly secrets:
    | NonNullable<PaymentProviderView["secrets"]>[Mode]
    | undefined;
  readonly isCurrent: boolean;
}

const SecretsOfMode = ({ mode, secrets, isCurrent }: SecretsOfModeProps) => {
  const { t } = useTranslation(["common", "admin"]);
  if (!secrets) {
    return null;
  }
  const rows = [
    { label: t("admin:payments.providers.secretKey"), ...secrets.secretKey },
    {
      label: t("admin:payments.providers.webhookSecret"),
      ...secrets.webhookSecret,
    },
  ];

  return (
    <div
      className={cn(
        "space-y-2 rounded-md border p-3",
        isCurrent && "border-primary/40",
      )}
    >
      <p className="text-sm font-medium">{t(`admin:payments.mode.${mode}`)}</p>
      <dl className="grid grid-cols-[1fr_auto] gap-x-4 gap-y-1 text-sm">
        {rows.map((row) => (
          <div key={row.name} className="contents">
            <dt>
              {row.label}{" "}
              <code className="text-muted-foreground text-xs">{row.name}</code>
            </dt>
            <dd
              className={cn(
                "text-right",
                row.status === "present" ? "text-success" : "text-destructive",
              )}
            >
              {t(`admin:payments.secret.${row.status}`)}
            </dd>
          </div>
        ))}
      </dl>
    </div>
  );
};
