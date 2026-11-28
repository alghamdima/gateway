import {
  infiniteQueryOptions,
  mutationOptions,
  queryOptions,
} from "@tanstack/react-query";

import {
  getInvitationsResponseSchema,
  getMembersResponseSchema,
  getOrganizationOrdersResponseSchema,
  getOrganizationSubscriptionsResponseSchema,
  getOrganizationResponseSchema,
  getUserAccountsResponseSchema,
  getUserInvitationsResponseSchema,
  getUserMembershipsResponseSchema,
  getUserOrdersResponseSchema,
  getUserSubscriptionsResponseSchema,
} from "@workspace/api/schema";
import { handle } from "@workspace/api/utils";

import { api } from "~/lib/api/client";
import { authClient } from "~/lib/auth/client";

import type { User } from "@workspace/auth";
import type { InferRequestType } from "hono/client";

const KEY = "admin";
const AUDIT_PAGE_SIZE = 100;

const queries = {
  users: {
    get: ({ id }: { id: string }) =>
      queryOptions({
        queryKey: [KEY, "users", id],
        queryFn: () =>
          authClient.admin.getUser({
            query: { id },
            fetchOptions: { throw: true },
          }) as Promise<User>,
      }),
    getAccounts: ({
      id,
      ...query
    }: InferRequestType<
      (typeof api.admin.users)[":id"]["accounts"]["$get"]
    >["query"] & { id: string }) => ({
      queryKey: [KEY, "users", id, "accounts", query],
      queryFn: () =>
        handle(api.admin.users[":id"].accounts.$get, {
          schema: getUserAccountsResponseSchema,
        })({
          query,
          param: {
            id,
          },
        }),
    }),
    getSubscriptions: ({
      id,
      ...query
    }: InferRequestType<
      (typeof api.admin.users)[":id"]["subscriptions"]["$get"]
    >["query"] & { id: string }) => ({
      queryKey: [KEY, "users", id, "subscriptions", query],
      queryFn: () =>
        handle(api.admin.users[":id"].subscriptions.$get, {
          schema: getUserSubscriptionsResponseSchema,
        })({ query, param: { id } }),
    }),
    getOrders: ({
      id,
      ...query
    }: InferRequestType<
      (typeof api.admin.users)[":id"]["orders"]["$get"]
    >["query"] & { id: string }) => ({
      queryKey: [KEY, "users", id, "orders", query],
      queryFn: () =>
        handle(api.admin.users[":id"].orders.$get, {
          schema: getUserOrdersResponseSchema,
        })({ query, param: { id } }),
    }),
    getMemberships: ({
      id,
      ...query
    }: InferRequestType<
      (typeof api.admin.users)[":id"]["memberships"]["$get"]
    >["query"] & { id: string }) =>
      queryOptions({
        queryKey: [KEY, "users", id, "memberships", query],
        queryFn: () =>
          handle(api.admin.users[":id"].memberships.$get, {
            schema: getUserMembershipsResponseSchema,
          })({ query, param: { id } }),
      }),
    getInvitations: ({
      id,
      ...query
    }: InferRequestType<
      (typeof api.admin.users)[":id"]["invitations"]["$get"]
    >["query"] & { id: string }) =>
      queryOptions({
        queryKey: [KEY, "users", id, "invitations", query],
        queryFn: () =>
          handle(api.admin.users[":id"].invitations.$get, {
            schema: getUserInvitationsResponseSchema,
          })({ query, param: { id } }),
      }),
    getSessions: ({ id }: { id: string }) =>
      queryOptions({
        queryKey: [KEY, "users", id, "sessions"],
        queryFn: () =>
          authClient.admin.listUserSessions({
            userId: id,
            fetchOptions: { throw: true },
          }),
      }),
  },
  organizations: {
    get: ({ id }: { id: string }) =>
      queryOptions({
        queryKey: [KEY, "organizations", id],
        queryFn: () =>
          handle(api.admin.organizations[":id"].$get, {
            schema: getOrganizationResponseSchema,
          })({
            param: { id },
          }),
      }),
    getMembers: ({
      id,
      ...query
    }: InferRequestType<
      (typeof api.admin.organizations)[":id"]["members"]["$get"]
    >["query"] & { id: string }) =>
      queryOptions({
        queryKey: [
          ...queries.organizations.get({ id }).queryKey,
          "members",
          query,
        ],
        queryFn: () =>
          handle(api.admin.organizations[":id"].members.$get, {
            schema: getMembersResponseSchema,
          })({
            query,
            param: {
              id,
            },
          }),
      }),
    getInvitations: ({
      id,
      ...query
    }: InferRequestType<
      (typeof api.admin.organizations)[":id"]["invitations"]["$get"]
    >["query"] & { id: string }) =>
      queryOptions({
        queryKey: [
          ...queries.organizations.get({ id }).queryKey,
          "invitations",
          query,
        ],
        queryFn: () =>
          handle(api.admin.organizations[":id"].invitations.$get, {
            schema: getInvitationsResponseSchema,
          })({ query, param: { id } }),
      }),
    getSubscriptions: ({
      id,
      ...query
    }: InferRequestType<
      (typeof api.admin.organizations)[":id"]["subscriptions"]["$get"]
    >["query"] & { id: string }) => ({
      queryKey: [
        ...queries.organizations.get({ id }).queryKey,
        "subscriptions",
        query,
      ],
      queryFn: () =>
        handle(api.admin.organizations[":id"].subscriptions.$get, {
          schema: getOrganizationSubscriptionsResponseSchema,
        })({ query, param: { id } }),
    }),
    getOrders: ({
      id,
      ...query
    }: InferRequestType<
      (typeof api.admin.organizations)[":id"]["orders"]["$get"]
    >["query"] & { id: string }) => ({
      queryKey: [
        ...queries.organizations.get({ id }).queryKey,
        "orders",
        query,
      ],
      queryFn: () =>
        handle(api.admin.organizations[":id"].orders.$get, {
          schema: getOrganizationOrdersResponseSchema,
        })({ query, param: { id } }),
    }),
  },
  audit: {
    list: () =>
      infiniteQueryOptions({
        queryKey: [KEY, "audit"],
        initialPageParam: undefined as number | undefined,
        queryFn: ({ pageParam }) =>
          handle(api.admin.audit.$get)({
            query: {
              limit: String(AUDIT_PAGE_SIZE),
              ...(pageParam === undefined ? {} : { before: String(pageParam) }),
            },
          }),
        getNextPageParam: (lastPage) =>
          lastPage.length === AUDIT_PAGE_SIZE ? lastPage.at(-1)?.id : undefined,
      }),
  },
  pricing: {
    get: () =>
      queryOptions({
        queryKey: [KEY, "pricing"],
        queryFn: () => handle(api.admin.pricing.$get)(),
      }),
    getChanges: () =>
      queryOptions({
        queryKey: [KEY, "pricing", "changes"],
        queryFn: () => handle(api.admin.pricing.changes.$get)(),
      }),
  },
  keySettings: {
    get: () =>
      queryOptions({
        queryKey: [KEY, "key-settings"],
        queryFn: () => handle(api.admin["key-settings"].$get)(),
      }),
    getChanges: () =>
      queryOptions({
        queryKey: [KEY, "key-settings", "changes"],
        queryFn: () => handle(api.admin["key-settings"].changes.$get)(),
      }),
  },
  accountSettings: {
    get: () =>
      queryOptions({
        queryKey: [KEY, "account-settings"],
        queryFn: () => handle(api.admin["account-settings"].$get)(),
      }),
    getChanges: () =>
      queryOptions({
        queryKey: [KEY, "account-settings", "changes"],
        queryFn: () => handle(api.admin["account-settings"].changes.$get)(),
      }),
    getUserLimit: (userId: string) =>
      queryOptions({
        queryKey: [KEY, "account-settings", "users", userId],
        queryFn: () =>
          handle(api.admin["account-settings"].users[":userId"].$get)({
            param: { userId },
          }),
      }),
  },
  payments: {
    get: () =>
      queryOptions({
        queryKey: [KEY, "payments"],
        queryFn: () => handle(api.admin.payments.$get)(),
      }),
    getChanges: () =>
      queryOptions({
        queryKey: [KEY, "payments", "changes"],
        queryFn: () => handle(api.admin.payments.changes.$get)(),
      }),
  },
  requests: {
    list: (
      query: InferRequestType<typeof api.admin.requests.$get>["query"] = {},
    ) =>
      queryOptions({
        queryKey: [KEY, "requests", query],
        queryFn: () => handle(api.admin.requests.$get)({ query }),
      }),
  },
  providers: {
    getCatalog: () =>
      queryOptions({
        queryKey: [KEY, "providers"],
        queryFn: () => handle(api.admin.providers.$get)(),
      }),
    getAccounts: () =>
      queryOptions({
        queryKey: [KEY, "providers", "accounts"],
        queryFn: () => handle(api.admin.providers.accounts.$get)(),
      }),
  },
};

const mutations = {
  users: {
    ban: mutationOptions({
      mutationKey: [KEY, "users", "ban"],
      mutationFn: (params: Parameters<typeof authClient.admin.banUser>[0]) =>
        authClient.admin.banUser(params),
    }),
    unban: mutationOptions({
      mutationKey: [KEY, "users", "unban"],
      mutationFn: (params: Parameters<typeof authClient.admin.unbanUser>[0]) =>
        authClient.admin.unbanUser(params),
    }),
    delete: mutationOptions({
      mutationKey: [KEY, "users", "delete"],
      mutationFn: (params: Parameters<typeof authClient.admin.removeUser>[0]) =>
        authClient.admin.removeUser(params),
    }),
    impersonate: mutationOptions({
      mutationKey: [KEY, "users", "impersonate"],
      mutationFn: (
        params: Parameters<typeof authClient.admin.impersonateUser>[0],
      ) => authClient.admin.impersonateUser(params),
    }),
    stopImpersonating: mutationOptions({
      mutationKey: [KEY, "users", "impersonate", "stop"],
      mutationFn: () => authClient.admin.stopImpersonating(),
    }),
    update: mutationOptions({
      mutationKey: [KEY, "users", "update"],
      mutationFn: (params: Parameters<typeof authClient.admin.updateUser>[0]) =>
        authClient.admin.updateUser(params),
    }),
    setPassword: mutationOptions({
      mutationKey: [KEY, "users", "setPassword"],
      mutationFn: (
        params: Parameters<typeof authClient.admin.setUserPassword>[0],
      ) => authClient.admin.setUserPassword(params),
    }),
    accounts: {
      delete: mutationOptions({
        mutationKey: [KEY, "users", "accounts", "delete"],
        mutationFn: (
          param: InferRequestType<
            (typeof api.admin.users)[":id"]["accounts"][":accountId"]["$delete"]
          >["param"],
        ) =>
          handle(api.admin.users[":id"].accounts[":accountId"].$delete)({
            param,
          }),
      }),
    },
    sessions: {
      revoke: mutationOptions({
        mutationKey: [KEY, "users", "sessions", "revoke"],
        mutationFn: (
          params: Parameters<typeof authClient.admin.revokeUserSession>[0],
        ) => authClient.admin.revokeUserSession(params),
      }),
      revokeAll: mutationOptions({
        mutationKey: [KEY, "users", "sessions", "revokeAll"],
        mutationFn: (
          params: Parameters<typeof authClient.admin.revokeUserSessions>[0],
        ) => authClient.admin.revokeUserSessions(params),
      }),
    },
  },
  organizations: {
    delete: mutationOptions({
      mutationKey: [KEY, "organizations", "delete"],
      mutationFn: (
        param: InferRequestType<
          (typeof api.admin.organizations)[":id"]["$delete"]
        >["param"],
      ) =>
        handle(api.admin.organizations[":id"].$delete)({
          param,
        }),
    }),
    update: mutationOptions({
      mutationKey: [KEY, "organizations", "update"],
      mutationFn: ({
        id,
        ...json
      }: InferRequestType<
        (typeof api.admin.organizations)[":id"]["$patch"]
      >["json"] & { id: string }) =>
        handle(api.admin.organizations[":id"].$patch)({ param: { id }, json }),
    }),
    members: {
      remove: mutationOptions({
        mutationKey: [KEY, "organizations", "members", "remove"],
        mutationFn: (
          param: InferRequestType<
            (typeof api.admin.organizations)[":id"]["members"][":memberId"]["$delete"]
          >["param"],
        ) =>
          handle(api.admin.organizations[":id"].members[":memberId"].$delete)({
            param,
          }),
      }),
      update: mutationOptions({
        mutationKey: [KEY, "organizations", "members", "update"],
        mutationFn: ({
          id,
          memberId,
          ...json
        }: InferRequestType<
          (typeof api.admin.organizations)[":id"]["members"][":memberId"]["$patch"]
        >["json"] & { id: string; memberId: string }) =>
          handle(api.admin.organizations[":id"].members[":memberId"].$patch)({
            param: { id, memberId },
            json,
          }),
      }),
    },
    invitations: {
      delete: mutationOptions({
        mutationKey: [KEY, "organizations", "invitations", "delete"],
        mutationFn: (
          param: InferRequestType<
            (typeof api.admin.organizations)[":id"]["invitations"][":invitationId"]["$delete"]
          >["param"],
        ) =>
          handle(
            api.admin.organizations[":id"].invitations[":invitationId"].$delete,
          )({
            param,
          }),
      }),
    },
  },
  pricing: {
    updateSettings: mutationOptions({
      mutationKey: [KEY, "pricing", "settings", "update"],
      mutationFn: (
        json: InferRequestType<typeof api.admin.pricing.settings.$put>["json"],
      ) => handle(api.admin.pricing.settings.$put)({ json }),
    }),
    updateCreditTerms: mutationOptions({
      mutationKey: [KEY, "pricing", "credit-terms", "update"],
      mutationFn: (
        json: InferRequestType<
          (typeof api.admin.pricing)["credit-terms"]["$put"]
        >["json"],
      ) => handle(api.admin.pricing["credit-terms"].$put)({ json }),
    }),
  },
  keySettings: {
    update: mutationOptions({
      mutationKey: [KEY, "key-settings", "update"],
      mutationFn: (
        json: InferRequestType<
          (typeof api.admin)["key-settings"]["$put"]
        >["json"],
      ) => handle(api.admin["key-settings"].$put)({ json }),
    }),
  },
  accountSettings: {
    update: mutationOptions({
      mutationKey: [KEY, "account-settings", "update"],
      mutationFn: (
        json: InferRequestType<
          (typeof api.admin)["account-settings"]["$put"]
        >["json"],
      ) => handle(api.admin["account-settings"].$put)({ json }),
    }),
    setUserLimit: mutationOptions({
      mutationKey: [KEY, "account-settings", "users", "update"],
      mutationFn: ({
        userId,
        ...json
      }: InferRequestType<
        (typeof api.admin)["account-settings"]["users"][":userId"]["$put"]
      >["json"] & { userId: string }) =>
        handle(api.admin["account-settings"].users[":userId"].$put)({
          param: { userId },
          json,
        }),
    }),
  },
  payments: {
    updateSettings: mutationOptions({
      mutationKey: [KEY, "payments", "settings", "update"],
      mutationFn: (
        json: InferRequestType<typeof api.admin.payments.settings.$put>["json"],
      ) => handle(api.admin.payments.settings.$put)({ json }),
    }),
    updateProvider: mutationOptions({
      mutationKey: [KEY, "payments", "providers", "update"],
      mutationFn: ({
        provider,
        ...json
      }: InferRequestType<
        (typeof api.admin.payments.providers)[":provider"]["$put"]
      >["json"] & {
        provider: InferRequestType<
          (typeof api.admin.payments.providers)[":provider"]["$put"]
        >["param"]["provider"];
      }) =>
        handle(api.admin.payments.providers[":provider"].$put)({
          param: { provider },
          json,
        }),
    }),
  },
  providers: {
    syncPrices: mutationOptions({
      mutationKey: [KEY, "providers", "price-sync"],
      mutationFn: () => handle(api.admin.providers["price-sync"].$post)(),
    }),
    createAccount: mutationOptions({
      mutationKey: [KEY, "providers", "accounts", "create"],
      mutationFn: (
        json: InferRequestType<
          typeof api.admin.providers.accounts.$post
        >["json"],
      ) => handle(api.admin.providers.accounts.$post)({ json }),
    }),
    deleteAccount: mutationOptions({
      mutationKey: [KEY, "providers", "accounts", "delete"],
      mutationFn: ({ provider }: { provider: string }) =>
        handle(api.admin.providers.accounts[":provider"].$delete)({
          param: { provider },
        }),
    }),
    refreshModels: mutationOptions({
      mutationKey: [KEY, "providers", "accounts", "refresh-models"],
      mutationFn: ({ provider }: { provider: string }) =>
        handle(
          api.admin.providers.accounts[":provider"]["refresh-models"].$post,
        )({ param: { provider } }),
    }),
    createKey: mutationOptions({
      mutationKey: [KEY, "providers", "accounts", "keys", "create"],
      mutationFn: ({
        provider,
        ...json
      }: InferRequestType<
        (typeof api.admin.providers.accounts)[":provider"]["keys"]["$post"]
      >["json"] & { provider: string }) =>
        handle(api.admin.providers.accounts[":provider"].keys.$post)({
          param: { provider },
          json,
        }),
    }),
    updateKey: mutationOptions({
      mutationKey: [KEY, "providers", "accounts", "keys", "update"],
      mutationFn: ({
        provider,
        keyId,
        ...json
      }: InferRequestType<
        (typeof api.admin.providers.accounts)[":provider"]["keys"][":keyId"]["$put"]
      >["json"] & { provider: string; keyId: string }) =>
        handle(api.admin.providers.accounts[":provider"].keys[":keyId"].$put)({
          param: { provider, keyId },
          json,
        }),
    }),
    deleteKey: mutationOptions({
      mutationKey: [KEY, "providers", "accounts", "keys", "delete"],
      mutationFn: ({ provider, keyId }: { provider: string; keyId: string }) =>
        handle(
          api.admin.providers.accounts[":provider"].keys[":keyId"].$delete,
        )({ param: { provider, keyId } }),
    }),
    setTerms: mutationOptions({
      mutationKey: [KEY, "providers", "terms", "set"],
      mutationFn: ({
        keyId,
        ...json
      }: InferRequestType<
        (typeof api.admin.providers)[":keyId"]["terms"]["$put"]
      >["json"] & { keyId: string }) =>
        handle(api.admin.providers[":keyId"].terms.$put)({
          param: { keyId },
          json,
        }),
    }),
    removeTerms: mutationOptions({
      mutationKey: [KEY, "providers", "terms", "remove"],
      mutationFn: ({ keyId }: { keyId: string }) =>
        handle(api.admin.providers[":keyId"].terms.$delete)({
          param: { keyId },
        }),
    }),
  },
};

export const admin = {
  queries,
  mutations,
} as const;
