"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { setDataPolicySchema } from "@workspace/api/schema";
import { handle } from "@workspace/api/utils";
import { toMemberRole } from "@workspace/auth";
import { useTranslation } from "@workspace/i18n";
import { Badge } from "@workspace/ui-web/badge";
import { Button } from "@workspace/ui-web/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@workspace/ui-web/card";
import { Checkbox } from "@workspace/ui-web/checkbox";
import { Skeleton } from "@workspace/ui-web/skeleton";
import { Spinner } from "@workspace/ui-web/spinner";
import { Switch } from "@workspace/ui-web/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@workspace/ui-web/table";
import { toast } from "@workspace/ui-web/toast";

import { api } from "~/lib/api/client";
import { authClient } from "~/lib/auth/client";
import { useActiveOrganization } from "~/modules/organization/hooks/use-active-organization";

import type * as z from "zod";

type Policy = z.infer<typeof setDataPolicySchema>;
type Region = NonNullable<Policy["allowedRegions"]>[number];

interface Endpoint {
  provider: string;
  region: string | null;
  isZeroRetention: boolean;
  isNeverTrainedOn: boolean;
  isAllowed: boolean;
}

const REGIONS =
  setDataPolicySchema.shape.allowedRegions.unwrap().element.options;

/** The same rule the API applies, to show what a change would allow before it is saved. */
const allows = (policy: Policy, endpoint: Endpoint) =>
  (!policy.requireZeroRetention || endpoint.isZeroRetention) &&
  (!policy.forbidTraining || endpoint.isNeverTrainedOn) &&
  (policy.allowedRegions === null ||
    (endpoint.region !== null &&
      policy.allowedRegions.some((region) => region === endpoint.region))) &&
  (policy.allowedProviders === null ||
    policy.allowedProviders.includes(endpoint.provider));

const toggled = <T,>(list: T[], item: T, isOn: boolean) =>
  isOn
    ? [...list.filter((x) => x !== item), item]
    : list.filter((x) => x !== item);

interface DataPolicyPageProps {
  readonly organizationId: string;
}

/** Where the organization's requests may go, enforced on every key (ADR 0015). */
export const DataPolicyPage = ({ organizationId }: DataPolicyPageProps) => {
  const { t } = useTranslation(["common", "organization"]);
  const queryClient = useQueryClient();
  const { activeMember } = useActiveOrganization();
  const canUpdate = authClient.organization.checkRolePermission({
    permissions: { apiKey: ["update"] },
    role: toMemberRole(activeMember?.role),
  });
  const queryKey = ["data-policy", organizationId];

  const saved = useQuery({
    queryKey,
    queryFn: () =>
      handle(api["data-policy"][":organizationId"].$get)({
        param: { organizationId },
      }),
  });
  const [draft, setDraft] = useState<Policy | null>(null);

  const save = useMutation({
    mutationFn: (json: Policy) =>
      handle(api["data-policy"][":organizationId"].$put)({
        param: { organizationId },
        json,
      }),
    onSuccess: async () => {
      setDraft(null);
      await queryClient.invalidateQueries({ queryKey });
      toast.add({ title: t("organization:dataPolicy.saved"), type: "success" });
    },
    onError: () => queryClient.invalidateQueries({ queryKey }),
  });

  if (saved.isError) {
    return (
      <p className="text-destructive text-sm">
        {t("organization:dataPolicy.unavailable")}
      </p>
    );
  }
  if (!saved.data) {
    return <Skeleton className="h-64 w-full" />;
  }

  const policy: Policy = draft ?? {
    requireZeroRetention: saved.data.requireZeroRetention,
    forbidTraining: saved.data.forbidTraining,
    allowedRegions: saved.data.allowedRegions,
    allowedProviders: saved.data.allowedProviders,
  };
  const change = (patch: Partial<Policy>) => setDraft({ ...policy, ...patch });
  const endpoints: Endpoint[] = saved.data.endpoints;
  const providers = [...new Set(endpoints.map((e) => e.provider))].sort();
  const matching = endpoints.filter((endpoint) => allows(policy, endpoint));
  const isValid = setDataPolicySchema.safeParse(policy).success;
  const isDisabled = !canUpdate || save.isPending;

  return (
    <div className="@container flex w-full flex-col gap-6">
      <Card>
        <CardHeader>
          <CardTitle>{t("organization:dataPolicy.title")}</CardTitle>
          <CardDescription>
            {t("organization:dataPolicy.description")}
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-6">
          {!saved.data.isApplied && (
            <p role="alert" className="text-destructive text-sm">
              {t("organization:dataPolicy.notApplied")}
            </p>
          )}

          <Toggle
            id="policy-zero-retention"
            label={t("organization:dataPolicy.zeroRetention.label")}
            description={t("organization:dataPolicy.zeroRetention.description")}
            checked={policy.requireZeroRetention}
            disabled={isDisabled}
            onChange={(requireZeroRetention) =>
              change({ requireZeroRetention })
            }
          />
          <Toggle
            id="policy-no-training"
            label={t("organization:dataPolicy.noTraining.label")}
            description={t("organization:dataPolicy.noTraining.description")}
            checked={policy.forbidTraining}
            disabled={isDisabled}
            onChange={(forbidTraining) => change({ forbidTraining })}
          />

          <Toggle
            id="policy-limit-regions"
            label={t("organization:dataPolicy.regions.label")}
            description={t("organization:dataPolicy.regions.description")}
            checked={policy.allowedRegions !== null}
            disabled={isDisabled}
            onChange={(isOn) =>
              change({ allowedRegions: isOn ? ["sa"] : null })
            }
          />
          {policy.allowedRegions !== null && (
            <fieldset className="ml-12 flex flex-wrap gap-x-6 gap-y-2">
              <legend className="sr-only">
                {t("organization:dataPolicy.regions.label")}
              </legend>
              {REGIONS.map((region: Region) => (
                <Choice
                  key={region}
                  id={`policy-region-${region}`}
                  label={t(`organization:dataPolicy.regions.names.${region}`)}
                  checked={policy.allowedRegions?.includes(region) ?? false}
                  disabled={isDisabled}
                  onChange={(isOn) =>
                    change({
                      allowedRegions: toggled(
                        policy.allowedRegions ?? [],
                        region,
                        isOn,
                      ),
                    })
                  }
                />
              ))}
            </fieldset>
          )}

          <Toggle
            id="policy-limit-providers"
            label={t("organization:dataPolicy.providers.label")}
            description={t("organization:dataPolicy.providers.description")}
            checked={policy.allowedProviders !== null}
            disabled={isDisabled || providers.length === 0}
            onChange={(isOn) =>
              change({ allowedProviders: isOn ? providers : null })
            }
          />
          {policy.allowedProviders !== null && (
            <fieldset className="ml-12 flex flex-wrap gap-x-6 gap-y-2">
              <legend className="sr-only">
                {t("organization:dataPolicy.providers.label")}
              </legend>
              {providers.map((provider) => (
                <Choice
                  key={provider}
                  id={`policy-provider-${provider}`}
                  label={provider}
                  checked={policy.allowedProviders?.includes(provider) ?? false}
                  disabled={isDisabled}
                  onChange={(isOn) =>
                    change({
                      allowedProviders: toggled(
                        policy.allowedProviders ?? [],
                        provider,
                        isOn,
                      ),
                    })
                  }
                />
              ))}
            </fieldset>
          )}

          {matching.length === 0 && (
            <p role="alert" className="text-destructive text-sm">
              {t("organization:dataPolicy.nothingMatches")}
            </p>
          )}

          {canUpdate ? (
            <div className="flex flex-wrap items-center gap-3">
              <Button
                type="button"
                disabled={
                  save.isPending ||
                  !isValid ||
                  (draft === null && saved.data.isApplied)
                }
                onClick={() => save.mutate(policy)}
              >
                {save.isPending ? <Spinner /> : t("save")}
              </Button>
              {draft !== null && (
                <Button
                  type="button"
                  variant="ghost"
                  disabled={save.isPending}
                  onClick={() => setDraft(null)}
                >
                  {t("cancel")}
                </Button>
              )}
              {save.isError && (
                <p role="alert" className="text-destructive text-sm">
                  {t("organization:dataPolicy.saveFailed")}
                </p>
              )}
            </div>
          ) : (
            <p className="text-muted-foreground text-sm">
              {t("organization:dataPolicy.readOnly")}
            </p>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("organization:dataPolicy.endpoints.title")}</CardTitle>
          <CardDescription>
            {t("organization:dataPolicy.endpoints.description", {
              matching: matching.length,
              total: endpoints.length,
            })}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {endpoints.length === 0 ? (
            <p className="text-muted-foreground text-sm">
              {t("organization:dataPolicy.endpoints.empty")}
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>
                    {t("organization:dataPolicy.endpoints.provider")}
                  </TableHead>
                  <TableHead>
                    {t("organization:dataPolicy.endpoints.region")}
                  </TableHead>
                  <TableHead>
                    {t("organization:dataPolicy.endpoints.retention")}
                  </TableHead>
                  <TableHead>
                    {t("organization:dataPolicy.endpoints.training")}
                  </TableHead>
                  <TableHead>
                    {t("organization:dataPolicy.endpoints.status")}
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {endpoints.map((endpoint, index) => (
                  // endpoints carry no id: the list is fixed between loads
                  // oxlint-disable-next-line react/no-array-index-key
                  <TableRow key={`${endpoint.provider}-${index}`}>
                    <TableCell className="font-mono">
                      {endpoint.provider}
                    </TableCell>
                    <TableCell>
                      {endpoint.region === null
                        ? t("organization:dataPolicy.endpoints.unknown")
                        : t(
                            `organization:dataPolicy.regions.names.${endpoint.region as Region}`,
                          )}
                    </TableCell>
                    <TableCell>
                      {endpoint.isZeroRetention
                        ? t("organization:dataPolicy.endpoints.notKept")
                        : t("organization:dataPolicy.endpoints.mayKeep")}
                    </TableCell>
                    <TableCell>
                      {endpoint.isNeverTrainedOn
                        ? t("organization:dataPolicy.endpoints.neverTrains")
                        : t("organization:dataPolicy.endpoints.mayTrain")}
                    </TableCell>
                    <TableCell>
                      {allows(policy, endpoint) ? (
                        <Badge>
                          {t("organization:dataPolicy.endpoints.allowed")}
                        </Badge>
                      ) : (
                        <Badge variant="outline">
                          {t("organization:dataPolicy.endpoints.blocked")}
                        </Badge>
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </div>
  );
};

interface ToggleProps {
  readonly id: string;
  readonly label: string;
  readonly description: string;
  readonly checked: boolean;
  readonly disabled: boolean;
  readonly onChange: (checked: boolean) => void;
}

const Toggle = ({
  id,
  label,
  description,
  checked,
  disabled,
  onChange,
}: ToggleProps) => (
  <div className="flex items-start gap-3">
    <Switch
      id={id}
      checked={checked}
      disabled={disabled}
      onCheckedChange={onChange}
      aria-describedby={`${id}-help`}
      className="mt-0.5"
    />
    <div>
      <label htmlFor={id} className="font-medium">
        {label}
      </label>
      <p id={`${id}-help`} className="text-muted-foreground text-sm">
        {description}
      </p>
    </div>
  </div>
);

interface ChoiceProps {
  readonly id: string;
  readonly label: string;
  readonly checked: boolean;
  readonly disabled: boolean;
  readonly onChange: (checked: boolean) => void;
}

const Choice = ({ id, label, checked, disabled, onChange }: ChoiceProps) => (
  <div className="flex items-center gap-2">
    <Checkbox
      id={id}
      checked={checked}
      disabled={disabled}
      onCheckedChange={onChange}
    />
    <label htmlFor={id} className="text-sm">
      {label}
    </label>
  </div>
);
