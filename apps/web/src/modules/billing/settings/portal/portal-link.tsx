import { useMutation } from "@tanstack/react-query";
import { useRouter } from "next/navigation";
import React from "react";
import { toast } from "sonner";

import { MOBILE_STORE_LINKS, MobileStore } from "@workspace/billing";
import { useTranslation } from "@workspace/i18n";
import { cn } from "@workspace/ui";
import { Button, buttonVariants } from "@workspace/ui-web/button";
import { Icons } from "@workspace/ui-web/icons";

import { pathsConfig } from "~/config/paths";
import { billing } from "~/modules/billing/lib/api";
import { TurboLink } from "~/modules/common/turbo-link";

export const PortalLink = ({
  children,
  store,
  redirectUrl = pathsConfig.dashboard.user.settings.billing,
  variantId,
  ...props
}: React.ComponentProps<typeof Button> & {
  store?: string;
  redirectUrl?: string;
  variantId?: string;
}) => {
  const { t } = useTranslation("billing");
  const router = useRouter();

  const getPortal = useMutation({
    ...billing.mutations.portal.get,
    onSuccess: (data) => {
      if (!data.url) {
        return toast.error(t("error.portal"));
      }
      return router.push(data.url);
    },
  });

  if (store && Object.values(MobileStore).includes(store)) {
    const url = new URL(
      MOBILE_STORE_LINKS[store as keyof typeof MOBILE_STORE_LINKS],
    );

    if (store === MobileStore.PLAY_STORE && variantId) {
      url.searchParams.set("sku", variantId);
    }

    return (
      <TurboLink
        {...(props as React.ComponentProps<typeof TurboLink>)}
        href={url.toString()}
        className={cn(
          buttonVariants({ variant: props.variant, size: props.size }),
          props.className,
        )}
        target="_blank"
        rel="noopener noreferrer"
      >
        {children}
      </TurboLink>
    );
  }

  return (
    <Button
      {...props}
      onClick={() =>
        getPortal.mutate({
          query: {
            redirectUrl,
          },
        })
      }
      disabled={getPortal.isPending || props.disabled}
    >
      {getPortal.isPending ? (
        <Icons.Loader2 className="size-5 animate-spin" />
      ) : (
        children
      )}
    </Button>
  );
};
