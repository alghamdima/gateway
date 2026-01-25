"use client";

import {
  ACTIVE_SUBSCRIPTION_STATUSES,
  findPlanByVariantId,
  SubscriptionStatus,
} from "@workspace/billing";
import { isKey, useTranslation } from "@workspace/i18n";
import { Badge } from "@workspace/ui-web/badge";
import { Icons } from "@workspace/ui-web/icons";

import { FeaturesList } from "~/modules/billing/features-list";
import { PortalLink } from "~/modules/billing/settings/portal/portal-link";
import { BillingPeriodProgress } from "~/modules/billing/subscriptions/billing-period-progress";

import type { Subscription } from "@workspace/billing";

export const SubscriptionsListItem = ({
  subscription,
}: {
  subscription: Subscription;
}) => {
  const { t, i18n } = useTranslation(["common", "billing"]);

  const { plan } = findPlanByVariantId(subscription.variantId);

  const name = plan?.name;
  const description = plan?.description;
  const statusKey = `subscription.status.${subscription.status
    .toLowerCase()
    .replace(/_([a-z])/g, (_, letter: string) => letter.toUpperCase())}`;

  return (
    <div className="flex flex-col gap-4 p-6">
      <div className="flex flex-col items-start justify-start gap-1">
        <div className="flex items-center justify-start gap-2">
          {ACTIVE_SUBSCRIPTION_STATUSES.includes(subscription.status) ? (
            <Icons.BadgeCheck className="size-5" />
          ) : (
            <Icons.BadgeX className="size-5" />
          )}
          {name && (
            <span className="text-lg font-medium capitalize">
              {isKey(name, i18n, "billing") ? t(name) : name}
            </span>
          )}
          <Badge
            variant={
              subscription.status === SubscriptionStatus.ACTIVE
                ? "success"
                : subscription.status === SubscriptionStatus.TRIALING
                  ? "secondary"
                  : "destructive"
            }
          >
            {isKey(statusKey, i18n, "billing") ? t(statusKey) : statusKey}
          </Badge>
        </div>
        {description && (
          <p className="text-muted-foreground text-sm">
            {isKey(description, i18n, "billing") ? t(description) : description}
          </p>
        )}
      </div>
      <div className="flex flex-col gap-4">
        {plan?.id && <FeaturesList planId={plan.id} />}
        {ACTIVE_SUBSCRIPTION_STATUSES.includes(subscription.status) && (
          <BillingPeriodProgress subscription={subscription} />
        )}
      </div>

      <div className="flex flex-col gap-2">
        <PortalLink
          store={subscription.store}
          variantId={subscription.variantId}
          className="w-full gap-1"
          variant="outline"
        >
          <span>{t("manage")}</span>
          <Icons.ArrowUpRight className="size-4" />
        </PortalLink>

        <div className="flex w-full items-center justify-between gap-2">
          <span className="text-muted-foreground text-xs">
            {t("updatedAt")}
          </span>

          <span className="text-foreground text-xs">
            {subscription.updatedAt.toLocaleString(i18n.language)}
          </span>
        </div>
      </div>
    </div>
  );
};
