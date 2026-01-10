import { useMutation } from "@tanstack/react-query";
import { usePathname, useRouter } from "next/navigation";
import { toast } from "sonner";

import { getHigherPlans, getLowerPlans } from "@workspace/billing";
import { useTranslation } from "@workspace/i18n";

import { appConfig } from "~/config/app";
import { pathsConfig } from "~/config/paths";
import { billing } from "~/modules/billing/lib/api";

import type { User } from "@workspace/auth";
import type { BillingConfigPlan } from "@workspace/billing";

export const usePlan = (plan: BillingConfigPlan) => {
  const { t } = useTranslation("billing");
  const router = useRouter();
  const checkout = useMutation({
    ...billing.mutations.checkout.create,
    onSuccess: (data) => {
      if (!data.url) {
        return toast.error(t("error.checkout"));
      }
      return router.push(data.url);
    },
  });

  const getPortal = useMutation({
    ...billing.mutations.portal.get,
    onSuccess: (data) => {
      if (!data.url) {
        return toast.error(t("error.portal"));
      }
      return router.push(data.url);
    },
  });

  const pathname = usePathname();

  const features = [
    ...plan.features.filter(
      (f) => !getLowerPlans(plan.id).some((p) => p.features.includes(f)),
    ),
    ...(getHigherPlans(plan.id)[0]
      ?.features.filter((f) => !plan.features.includes(f))
      .slice(0, 3) ?? []),
  ].map((feature) => ({
    id: feature,
    available: plan.features.includes(feature),
    key: `feature.${feature.toLowerCase().replace(/_([a-z])/g, (_, letter: string) => letter.toUpperCase())}`,
  }));

  const handleCheckout = ({
    user,
    ...rest
  }: Omit<Parameters<typeof checkout.mutate>[0]["json"], "redirect"> & {
    user: User | null;
  }) => {
    if (!user) {
      const url = new URL(pathsConfig.auth.login, appConfig.url);
      url.searchParams.set("redirectTo", pathsConfig.marketing.pricing);
      return router.push(url.toString());
    }

    checkout.mutate({
      json: {
        ...rest,
        redirect: {
          success: `${appConfig.url}${pathsConfig.dashboard.user.index}`,
          cancel: `${appConfig.url}${pathname}`,
        },
      },
    });
  };

  const handleOpenPortal = ({ user }: { user: User | null }) => {
    if (!user) {
      const url = new URL(pathsConfig.auth.login, appConfig.url);
      url.searchParams.set("redirectTo", pathsConfig.marketing.pricing);
      return router.push(url.toString());
    }

    getPortal.mutate({
      query: {
        redirectUrl: `${appConfig.url}${pathname}`,
      },
    });
  };

  return {
    isPending: checkout.isPending || getPortal.isPending,
    // price,
    features,
    // discount,
    handleCheckout,
    handleOpenPortal,
  };
};
