import Constants from "expo-constants";
import * as Notifications from "expo-notifications";
import { useEffect, useState } from "react";

import { logger } from "@workspace/shared/logger";

import { useNotificationsPermissions } from "~/modules/common/hooks/notifications/use-notifications-permissions";
import { isAndroid } from "~/utils/device";

export const usePushToken = () => {
  const permissions = useNotificationsPermissions();

  const [token, setToken] = useState<string | null>(null);
  const [isLoading, setIsLoading] = useState(false);

  const getToken = async () => {
    setIsLoading(true);

    try {
      if (isAndroid) {
        await Notifications.setNotificationChannelAsync("default", {
          name: "Default",
          importance: Notifications.AndroidImportance.MAX,
        });
      }

      const pushToken = await Notifications.getExpoPushTokenAsync({
        projectId:
          Constants.expoConfig?.extra?.eas?.projectId ??
          Constants.easConfig?.projectId,
      });

      setToken(pushToken.data);
      return pushToken.data;
    } catch (error) {
      logger.error("Failed to get Expo push token:", error);
      setToken(null);
      throw error;
    } finally {
      setIsLoading(false);
    }
  };

  useEffect(() => {
    if (!permissions.isGranted) {
      setToken(null);
      return;
    }

    void getToken();
  }, [permissions.isGranted]);

  return {
    token,
    isLoading,
    getToken,
  };
};
