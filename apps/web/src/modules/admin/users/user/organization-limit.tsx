"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { useTranslation } from "@workspace/i18n";
import { Skeleton } from "@workspace/ui-web/skeleton";
import { toast } from "@workspace/ui-web/toast";

import { OrganizationLimitForm } from "~/modules/admin/account-settings/organization-limit-form";
import { admin } from "~/modules/admin/lib/api";
import { useStaffPermission } from "~/modules/admin/lib/use-staff-permission";

interface UserOrganizationLimitProps {
  readonly userId: string;
}

/**
 * How many organizations this user may own (ADR 0020): the general limit, or
 * their own when staff set one here.
 */
export const UserOrganizationLimit = ({
  userId,
}: UserOrganizationLimitProps) => {
  const { t } = useTranslation(["common", "admin"]);
  const queryClient = useQueryClient();
  const canUpdate = useStaffPermission({ organizations: ["update"] });
  const query = admin.queries.accountSettings.getUserLimit(userId);
  const limit = useQuery(query);

  const save = useMutation({
    ...admin.mutations.accountSettings.setUserLimit,
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: query.queryKey });
      toast.add({
        title: t("admin:accountSettings.user.saved"),
        type: "success",
      });
    },
  });

  if (!limit.data) {
    return <Skeleton className="h-14 w-full" />;
  }

  const figures = [
    { label: t("admin:accountSettings.user.owned"), value: limit.data.owned },
    {
      label: t("admin:accountSettings.user.effective"),
      value: limit.data.effective,
    },
    {
      label: t("admin:accountSettings.user.own"),
      value: limit.data.own ?? t("admin:accountSettings.limit.general"),
    },
  ];

  return (
    <div className="flex flex-col gap-6">
      <dl className="grid gap-4 sm:grid-cols-3">
        {figures.map((figure) => (
          <div key={figure.label}>
            <dt className="text-muted-foreground text-sm">{figure.label}</dt>
            <dd className="text-xl font-semibold tabular-nums">
              {figure.value}
            </dd>
          </div>
        ))}
      </dl>
      {canUpdate && (
        <OrganizationLimitForm
          id="admin-user-organization-limit-form"
          value={limit.data.own}
          isOptional
          description={t("admin:accountSettings.user.help")}
          onSave={(value) =>
            save.mutateAsync({ userId, maxOwnedOrganizations: value })
          }
        />
      )}
    </div>
  );
};
