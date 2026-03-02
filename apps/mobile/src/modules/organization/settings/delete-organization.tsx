import { useMutation } from "@tanstack/react-query";
import { router } from "expo-router";
import { Alert } from "react-native";

import { toMemberRole } from "@workspace/auth";
import { useTranslation } from "@workspace/i18n";
import { Icons } from "@workspace/ui-mobile/icons";
import { Text } from "@workspace/ui-mobile/text";

import { pathsConfig } from "~/config/paths";
import { authClient } from "~/lib/auth";
import { SettingsTile } from "~/modules/common/settings-tile";
import { Spinner } from "~/modules/common/spinner";
import { organization } from "~/modules/organization/lib/api";

export const DeleteOrganization = () => {
  const { t } = useTranslation(["common", "organization"]);
  const { data: activeOrganization } = authClient.useActiveOrganization();
  const { data: activeMember } = authClient.useActiveMember();

  const deleteOrganization = useMutation({
    ...organization.mutations.delete,
    onSuccess: () => {
      router.replace(pathsConfig.dashboard.user.index);
    },
  });

  const hasDeletePermission = authClient.organization.checkRolePermission({
    permissions: {
      organization: ["delete"],
    },
    role: toMemberRole(activeMember?.role),
  });

  if (!activeOrganization || !hasDeletePermission) {
    return null;
  }

  return (
    <>
      <SettingsTile
        icon={Icons.Trash2}
        destructive
        onPress={() => {
          Alert.alert(t("delete.title"), t("delete.disclaimer"), [
            {
              text: t("cancel"),
              style: "cancel",
            },
            {
              text: t("delete.title"),
              style: "destructive",
              onPress: () =>
                deleteOrganization.mutate({
                  organizationId: activeOrganization.id,
                }),
            },
          ]);
        }}
      >
        <Text>{t("delete.title")}</Text>
      </SettingsTile>
      {deleteOrganization.isPending && <Spinner />}
    </>
  );
};
