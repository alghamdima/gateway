import { View } from "react-native";

import {
  ACTIVE_SUBSCRIPTION_STATUSES,
  findPlanByVariantId,
  MobileStore,
  SubscriptionStatus,
} from "@workspace/billing";
import { isKey, useTranslation } from "@workspace/i18n";
import { cn } from "@workspace/ui";
import { Badge } from "@workspace/ui-mobile/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from "@workspace/ui-mobile/card";
import { Icons } from "@workspace/ui-mobile/icons";
import { Text } from "@workspace/ui-mobile/text";

import { FeaturesList } from "~/modules/billing/features-list";
import { NativePortalLink } from "~/modules/billing/portal/native-portal-link";
import { WebPortalLink } from "~/modules/billing/portal/web-portal-link";

import { BillingPeriodProgress } from "../billing-period-progress";

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
    <Card>
      <CardHeader>
        <View className="flex-row items-center justify-start gap-2">
          {ACTIVE_SUBSCRIPTION_STATUSES.includes(subscription.status) ? (
            <Icons.BadgeCheck size={20} className="text-foreground" />
          ) : (
            <Icons.BadgeX size={20} className="text-foreground" />
          )}
          {name && (
            <CardTitle>
              {isKey(name, i18n, "billing") ? t(name) : name}
            </CardTitle>
          )}
          <Badge
            className={cn(
              "bg-destructive/15 active:bg-destructive/25 ml-1 capitalize",
              {
                "bg-success/15 active:bg-success/25":
                  subscription.status === SubscriptionStatus.ACTIVE,
                "bg-muted active:bg-muted/25":
                  subscription.status === SubscriptionStatus.TRIALING,
              },
            )}
          >
            <Text
              className={cn("text-destructive", {
                "text-success":
                  subscription.status === SubscriptionStatus.ACTIVE,
                "text-muted-foreground":
                  subscription.status === SubscriptionStatus.TRIALING,
              })}
            >
              {isKey(statusKey, i18n, "billing") ? t(statusKey) : statusKey}
            </Text>
          </Badge>
        </View>
        {description && (
          <CardDescription>
            {isKey(description, i18n, "billing") ? t(description) : description}
          </CardDescription>
        )}
      </CardHeader>
      <CardContent className="gap-5">
        {plan?.id && <FeaturesList planId={plan.id} />}
        {ACTIVE_SUBSCRIPTION_STATUSES.includes(subscription.status) && (
          <BillingPeriodProgress subscription={subscription} />
        )}
      </CardContent>

      <CardFooter className="flex-col gap-2">
        {Object.values(MobileStore).includes(subscription.store) ? (
          <NativePortalLink
            store={subscription.store}
            variantId={subscription.variantId}
            variant="outline"
            className="w-full gap-1"
          >
            <Text>{t("manage")}</Text>
            <Icons.ArrowUpRight className="text-foreground" size={18} />
          </NativePortalLink>
        ) : (
          <WebPortalLink variant="outline" className="w-full gap-1">
            <Text>{t("manage")}</Text>
            <Icons.ArrowUpRight className="text-foreground" size={18} />
          </WebPortalLink>
        )}

        <View className="w-full flex-row items-center justify-between gap-2">
          <Text className="text-muted-foreground text-xs">
            {t("updatedAt")}
          </Text>

          <Text className="text-foreground text-xs">
            {subscription.updatedAt.toLocaleString(i18n.language)}
          </Text>
        </View>
      </CardFooter>
    </Card>
  );
};
