import { mutationOptions, queryOptions } from "@tanstack/react-query";

import { handle } from "@workspace/api/utils";

import { api } from "~/lib/api/client";

import type { InferRequestType } from "hono/client";

const KEY = "keys";

type KeysRoute = (typeof api.keys)[":organizationId"];

const queries = {
  list: (organizationId: string) =>
    queryOptions({
      queryKey: [KEY, organizationId],
      queryFn: () =>
        handle(api.keys[":organizationId"].$get)({
          param: { organizationId },
        }),
    }),
};

const mutations = {
  create: mutationOptions({
    mutationKey: [KEY, "create"],
    mutationFn: ({
      organizationId,
      ...json
    }: InferRequestType<KeysRoute["$post"]>["json"] & {
      organizationId: string;
    }) =>
      handle(api.keys[":organizationId"].$post)({
        param: { organizationId },
        json,
      }),
  }),
  update: mutationOptions({
    mutationKey: [KEY, "update"],
    mutationFn: ({
      organizationId,
      keyId,
      ...json
    }: InferRequestType<KeysRoute[":keyId"]["$patch"]>["json"] & {
      organizationId: string;
      keyId: string;
    }) =>
      handle(api.keys[":organizationId"][":keyId"].$patch)({
        param: { organizationId, keyId },
        json,
      }),
  }),
  delete: mutationOptions({
    mutationKey: [KEY, "delete"],
    mutationFn: (param: { organizationId: string; keyId: string }) =>
      handle(api.keys[":organizationId"][":keyId"].$delete)({ param }),
  }),
};

export const keys = { queries, mutations } as const;
