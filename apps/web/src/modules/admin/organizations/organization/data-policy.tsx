"use client";

import { useQuery } from "@tanstack/react-query";

import { handle } from "@workspace/api/utils";
import { useTranslation } from "@workspace/i18n";
import { Badge } from "@workspace/ui-web/badge";
import { Skeleton } from "@workspace/ui-web/skeleton";

import { api } from "~/lib/api/client";

interface OrganizationDataPolicyProps {
  readonly organizationId: string;
}

/**
 * The organization's data policy, for staff to read (ADR 0015). Its owners
 * and admins set it in their dashboard; staff change what it can reach by
 * recording provider terms.
 */
export const OrganizationDataPolicy = ({
  organizationId,
}: OrganizationDataPolicyProps) => {
  const { t } = useTranslation(["admin", "organization"]);
  const policy = useQuery({
    queryKey: ["admin", "organizations", organizationId, "data-policy"],
    queryFn: () =>
      handle(api.admin.organizations[":id"]["data-policy"].$get)({
        param: { id: organizationId },
      }),
  });

  if (policy.isError) {
    return (
      <p className="text-destructive text-sm">
        {t("organization:dataPolicy.unavailable")}
      </p>
    );
  }
  if (!policy.data) {
    return <Skeleton className="h-24 w-full" />;
  }

  const { data } = policy;
  const regionName = (region: string) =>
    t(`organization:dataPolicy.regions.names.${region as "sa"}`);
  const requirements = [
    data.requireZeroRetention &&
      t("organization:dataPolicy.zeroRetention.label"),
    data.forbidTraining && t("organization:dataPolicy.noTraining.label"),
    data.allowedRegions?.map(regionName).join(", "),
    data.allowedProviders?.join(", "),
  ].filter((item): item is string => Boolean(item));
  const allowed = data.endpoints.filter((endpoint) => endpoint.isAllowed);

  return (
    <div className="space-y-3 text-sm">
      {requirements.length > 0 ? (
        <ul className="list-disc space-y-1 pl-5">
          {requirements.map((requirement) => (
            <li key={requirement}>{requirement}</li>
          ))}
        </ul>
      ) : (
        <p>{t("admin:organizations.organization.dataPolicy.open")}</p>
      )}
      <div className="flex flex-wrap items-center gap-2">
        {data.version > 0 && (
          <span className="text-muted-foreground">
            {t("admin:organizations.organization.dataPolicy.version", {
              version: data.version,
            })}
          </span>
        )}
        {data.isApplied ? (
          <Badge variant="outline">
            {t("admin:organizations.organization.dataPolicy.applied")}
          </Badge>
        ) : (
          <Badge variant="destructive">
            {t("admin:organizations.organization.dataPolicy.notApplied")}
          </Badge>
        )}
      </div>
      <p className="text-muted-foreground">
        {t("organization:dataPolicy.endpoints.description", {
          matching: allowed.length,
          total: data.endpoints.length,
        })}
      </p>
      {allowed.length === 0 && data.endpoints.length > 0 && (
        <p role="alert" className="text-destructive">
          {t("admin:organizations.organization.dataPolicy.nothingAllowed")}
        </p>
      )}
    </div>
  );
};
