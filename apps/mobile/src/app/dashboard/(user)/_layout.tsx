import { Tabs } from "expo-router";

import { useTranslation } from "@workspace/i18n";
import { cn } from "@workspace/ui";
import { Icons } from "@workspace/ui-mobile/icons";

import { UserHeader } from "~/modules/common/layout/header";
import { TabBarLabel } from "~/modules/common/styled";

export const unstable_settings = {
  anchor: "index",
};

export default function UserLayout() {
  const { t } = useTranslation("common");

  return (
    <Tabs
      screenOptions={{
        tabBarStyle: {
          paddingTop: 4,
        },
        animation: "fade",
      }}
    >
      <Tabs.Screen
        name="index"
        options={{
          header: () => <UserHeader />,
          title: t("home"),
          tabBarIcon: ({ focused }) => (
            <Icons.House
              size={24}
              className={cn("text-muted-foreground", {
                "text-primary": focused,
              })}
            />
          ),
          tabBarLabel: TabBarLabel,
        }}
      />
      <Tabs.Screen
        name="ai"
        activityEnabled
        options={{
          headerShown: false,
          title: t("ai"),
          tabBarIcon: ({ focused }) => (
            <Icons.WandSparkles
              size={24}
              className={cn("text-muted-foreground", {
                "text-primary": focused,
              })}
            />
          ),
          tabBarLabel: TabBarLabel,
        }}
      />
      <Tabs.Screen
        name="settings"
        options={{
          headerShown: false,
          title: t("settings"),
          tabBarIcon: ({ focused }) => (
            <Icons.Settings
              size={24}
              className={cn("text-muted-foreground", {
                "text-primary": focused,
              })}
            />
          ),
          tabBarLabel: TabBarLabel,
        }}
      />
    </Tabs>
  );
}
