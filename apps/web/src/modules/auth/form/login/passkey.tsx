"use client";

import { useMutation } from "@tanstack/react-query";
import { useRouter } from "next/navigation";
import { useEffect } from "react";

import { AuthProvider } from "@workspace/auth";
import { useTranslation } from "@workspace/i18n";
import { Badge } from "@workspace/ui-web/badge";
import { Button } from "@workspace/ui-web/button";
import { Icons } from "@workspace/ui-web/icons";
import { Spinner } from "@workspace/ui-web/spinner";

import { pathsConfig } from "~/config/paths";
import { authClient } from "~/lib/auth/client";

import { auth } from "../../lib/api";

interface PasskeyLoginProps {
  readonly redirectTo?: string;
}

export const PasskeyLogin = ({
  redirectTo = pathsConfig.dashboard.user.index,
}: PasskeyLoginProps) => {
  const router = useRouter();
  const { t } = useTranslation(["auth", "common"]);

  const { mutate: signInWithPasskey, isPending } = useMutation({
    ...auth.mutations.signIn.passkey,
    onSuccess: () => {
      router.replace(redirectTo);
    },
  });

  useEffect(() => {
    signInWithPasskey({ autoFill: true });
  }, [signInWithPasskey]);

  return (
    <Button
      variant="outline"
      className="relative gap-2"
      size="lg"
      onClick={() => signInWithPasskey(undefined)}
      disabled={isPending}
    >
      {isPending ? (
        <Spinner className="size-4" />
      ) : (
        <>
          <Icons.Key className="size-4" />
          {t("login.passkey.cta")}
        </>
      )}

      {authClient.isLastUsedLoginMethod(AuthProvider.PASSKEY) && (
        <Badge className="absolute top-0 -right-4 z-10 -translate-y-1/2 shadow-sm">
          {t("lastUsed")}
        </Badge>
      )}
    </Button>
  );
};
