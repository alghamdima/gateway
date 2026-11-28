"use client";

import { hasStaffPermission } from "@workspace/auth";

import { authClient } from "~/lib/auth/client";

import type { StaffPermissions } from "@workspace/auth";

/**
 * Whether the signed-in staff member may do this. Only hides what they cannot
 * do: the API checks the same permissions (packages/api/src/modules/admin/access.ts,
 * and Better Auth's admin plugin for /api/auth/admin/*).
 */
export const useStaffPermission = (permissions: StaffPermissions) => {
  const { data } = authClient.useSession();
  return data?.user ? hasStaffPermission(data.user, permissions) : false;
};
