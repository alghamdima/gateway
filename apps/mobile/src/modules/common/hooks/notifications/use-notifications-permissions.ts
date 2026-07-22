import * as Notifications from "expo-notifications";
import { useEffect, useState } from "react";
import { AppState } from "react-native";

import { logger } from "@workspace/shared/logger";

import { isAndroid } from "~/utils/device";

export const useNotificationsPermissions = () => {
  const [status, setStatus] = useState<Notifications.PermissionStatus | null>(
    null,
  );
  const [isLoading, setIsLoading] = useState(false);

  const refresh = async () => {
    try {
      const permission = await Notifications.getPermissionsAsync();
      setStatus(permission.status);
      return permission.status;
    } catch (error) {
      logger.error("Failed to get notification permissions:", error);
      throw error;
    }
  };

  useEffect(() => {
    void refresh();

    const subscription = AppState.addEventListener("change", (state) => {
      if (state === "active") {
        void refresh();
      }
    });

    return () => subscription.remove();
  }, []);

  const request = async () => {
    setIsLoading(true);

    try {
      if (isAndroid) {
        await Notifications.setNotificationChannelAsync("default", {
          name: "Default",
          importance: Notifications.AndroidImportance.MAX,
        });
      }

      const existing = await Notifications.getPermissionsAsync();

      if (existing.status === Notifications.PermissionStatus.GRANTED) {
        setStatus(existing.status);
        return existing.status;
      }

      const requested = await Notifications.requestPermissionsAsync();
      setStatus(requested.status);
      return requested.status;
    } finally {
      setIsLoading(false);
    }
  };

  return {
    status,
    isGranted: status === Notifications.PermissionStatus.GRANTED,
    isLoading,
    request,
    refresh,
  };
};
