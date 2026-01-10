import { View } from "react-native";

import {
  findPlanByVariantId,
  MobileStore,
  PaymentStatus,
} from "@workspace/billing";
import { isKey, useTranslation } from "@workspace/i18n";
import { cn } from "@workspace/ui";
import { Badge } from "@workspace/ui-mobile/badge";
import {
  Card,
  CardHeader,
  CardTitle,
  CardDescription,
  CardFooter,
  CardContent,
} from "@workspace/ui-mobile/card";
import { Icons } from "@workspace/ui-mobile/icons";
import { Text } from "@workspace/ui-mobile/text";

import { FeaturesList } from "~/modules/billing/features-list";
import { NativePortalLink } from "~/modules/billing/portal/native-portal-link";
import { WebPortalLink } from "~/modules/billing/portal/web-portal-link";

import type { Order } from "@workspace/billing";

export const OrdersListItem = ({ order }: { order: Order }) => {
  const { t, i18n } = useTranslation(["common", "billing"]);
  const { plan } = findPlanByVariantId(order.variantId);

  const name = plan?.name;
  const description = plan?.description;

  return (
    <Card>
      <CardHeader>
        <View className="flex-row items-center justify-start gap-2">
          {order.status === PaymentStatus.SUCCEEDED ? (
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
                  order.status === PaymentStatus.SUCCEEDED,
                "bg-muted active:bg-muted/25":
                  order.status === PaymentStatus.FAILED,
              },
            )}
          >
            <Text
              className={cn("text-destructive", {
                "text-success": order.status === PaymentStatus.SUCCEEDED,
                "text-muted-foreground": order.status === PaymentStatus.FAILED,
              })}
            >
              {t(`order.status.${order.status}`)}
            </Text>
          </Badge>
        </View>
        {description && (
          <CardDescription>
            {isKey(description, i18n, "billing") ? t(description) : description}
          </CardDescription>
        )}
      </CardHeader>

      {plan?.id && (
        <CardContent>
          <FeaturesList planId={plan.id} />
        </CardContent>
      )}

      <CardFooter className="flex-col gap-2">
        {Object.values(MobileStore).includes(order.store) ? (
          <NativePortalLink
            store={order.store}
            variantId={order.variantId}
            variant="outline"
            className="w-full gap-1"
          >
            <Text>{t("order.cta")}</Text>
            <Icons.ArrowUpRight className="text-foreground" size={18} />
          </NativePortalLink>
        ) : (
          <WebPortalLink variant="outline" className="w-full gap-1">
            <Text>{t("order.cta")}</Text>
            <Icons.ArrowUpRight className="text-foreground" size={18} />
          </WebPortalLink>
        )}

        <View className="w-full flex-row items-center justify-between gap-2">
          <Text className="text-muted-foreground text-xs">
            {t("updatedAt")}
          </Text>

          <Text className="text-foreground text-xs">
            {order.updatedAt.toLocaleString(i18n.language)}
          </Text>
        </View>
      </CardFooter>
    </Card>
  );
};
