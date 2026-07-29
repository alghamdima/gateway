import { View } from "react-native";

import { BuiltWith } from "@workspace/ui-mobile/built-with";
import { Text } from "@workspace/ui-mobile/text";

import { appConfig } from "~/config/app";
import { ScrollView } from "~/modules/common/styled";
import { AreaChart } from "~/modules/organization/home/charts/area";
import { BarChart } from "~/modules/organization/home/charts/bar";
import { PieChart } from "~/modules/organization/home/charts/pie";

export default function Home() {
  return (
    <ScrollView
      className="bg-background"
      contentContainerClassName="gap-4 items-center bg-background px-6 py-2"
      showsVerticalScrollIndicator={false}
    >
      <BarChart />
      <PieChart />
      <AreaChart />

      <View className="items-center gap-1.5 pt-4 pb-10">
        <BuiltWith />
        {appConfig.version ? (
          <Text className="text-muted-foreground text-xs">
            {`v${appConfig.version}`}
          </Text>
        ) : null}
      </View>
    </ScrollView>
  );
}
