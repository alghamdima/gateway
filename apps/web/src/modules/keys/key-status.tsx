"use client";

import { useTranslation } from "@workspace/i18n";
import { Badge } from "@workspace/ui-web/badge";

/** Enabled, disabled or expired, as the API works it out (KEYS-04). */
export type KeyStatus = "enabled" | "disabled" | "expired";

const VARIANT = {
  enabled: "success",
  disabled: "outline",
  expired: "destructive",
} as const;

export const KeyStatusBadge = ({ status }: { readonly status: KeyStatus }) => {
  const { t } = useTranslation("organization");
  return <Badge variant={VARIANT[status]}>{t(`keys.status.${status}`)}</Badge>;
};

/** An expiry under an hour away is shown in amber, as OpenRouter does. */
export const EXPIRES_SOON_MS = 3_600_000;

export const expiresSoon = (expiresAt: string | null, now: number) =>
  expiresAt !== null &&
  new Date(expiresAt).getTime() > now &&
  new Date(expiresAt).getTime() - now < EXPIRES_SOON_MS;
