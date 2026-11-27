import { mutationOptions, queryOptions } from "@tanstack/react-query";

import { handle } from "@workspace/api/utils";

import { api } from "~/lib/api/client";

import type { InferRequestType, InferResponseType } from "hono/client";

const KEY = "keys";

type KeysRoute = (typeof api.keys)[":organizationId"];

export type KeyListRow = InferResponseType<KeysRoute["$get"], 200>[number];
export type KeyDetail = InferResponseType<KeysRoute[":keyId"]["$get"], 200>;
export type KeyCreationSettings = InferResponseType<
  KeysRoute["settings"]["$get"],
  200
>;

const queries = {
  list: (organizationId: string) =>
    queryOptions({
      queryKey: [KEY, organizationId],
      queryFn: () =>
        handle(api.keys[":organizationId"].$get)({
          param: { organizationId },
        }),
    }),
  /** What the create form starts from and allows (ADMIN-12). */
  settings: (organizationId: string) =>
    queryOptions({
      queryKey: [KEY, organizationId, "settings"],
      queryFn: () =>
        handle(api.keys[":organizationId"].settings.$get)({
          param: { organizationId },
        }),
    }),
  detail: (organizationId: string, keyId: string) =>
    queryOptions({
      queryKey: [KEY, organizationId, "detail", keyId],
      queryFn: () =>
        handle(api.keys[":organizationId"][":keyId"].$get)({
          param: { organizationId, keyId },
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
  /** A pasted key goes in the body, never in a URL (KEYS-05). */
  search: mutationOptions({
    mutationKey: [KEY, "search"],
    mutationFn: ({
      organizationId,
      query,
    }: {
      organizationId: string;
      query: string;
    }) =>
      handle(api.keys[":organizationId"].search.$post)({
        param: { organizationId },
        json: { query },
      }),
  }),
  update: mutationOptions({
    mutationKey: [KEY, "update"],
    mutationFn: ({
      organizationId,
      keyId,
      ...json
    }: {
      organizationId: string;
      keyId: string;
      name?: string;
      disabled?: boolean;
      limit?: InferRequestType<KeysRoute["$post"]>["json"]["limit"];
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
