import * as z from "zod";

import {
  getOrigin,
  matchesPattern,
  mergeSearchParams,
} from "@workspace/shared/utils";

import { MemberRole, UserRole } from "../types";

import type { User } from "../types";

export const trustedOrigins = [
  /* Needed only for Apple ID authentication */
  "https://appleid.apple.com",
  ...(process.env.NODE_ENV !== "production"
    ? ["http://localhost*", "https://localhost*"]
    : []),
];

const isTrustedOrigin = (candidate: string, request?: Request) => {
  try {
    const _url = new URL(candidate);
  } catch {
    return false;
  }

  const origins = [
    ...trustedOrigins,
    ...(request?.url ? [new URL(request.url).origin] : []),
  ];

  return origins.some((origin) => matchesPattern(candidate, origin));
};

export const getUrl = ({
  request,
  url,
  type,
}: {
  request?: Request;
  url?: string;
  type?: string;
}) => {
  const passedUrl = request?.headers.get("x-url");

  let resultUrl: URL;

  if (passedUrl && isTrustedOrigin(passedUrl, request)) {
    // Base on x-url; merge in params from provided `url` without overwriting existing keys
    resultUrl = new URL(passedUrl);
    if (url) {
      const urlObj = new URL(url, getOrigin(resultUrl.toString()) ?? undefined);
      mergeSearchParams(resultUrl, urlObj, { overwrite: false });
    }
  } else {
    // For web, use the provided URL or fall back to the request URL
    resultUrl = new URL(url ?? request?.url ?? "");
  }

  if (type) {
    resultUrl.searchParams.set("type", type);
  }

  return resultUrl;
};

const hierarchy: MemberRole[] = [
  MemberRole.MEMBER,
  MemberRole.ADMIN,
  MemberRole.OWNER,
];

export const generateName = (email?: string) => {
  return email?.split("@")[0]?.trim() ?? "Anonymous";
};

export const getAllRolesAtOrBelow = (role: MemberRole): MemberRole[] => {
  const idx = hierarchy.indexOf(role);
  if (idx === -1) return [];

  return hierarchy.slice(0, idx + 1);
};

export const getAllRolesAtOrAbove = (role: MemberRole): MemberRole[] => {
  const idx = hierarchy.indexOf(role);
  if (idx === -1) return [];
  return hierarchy.slice(idx, hierarchy.length);
};

export const hasAdminPermission = (user: User) =>
  user.role?.split(",").includes(UserRole.ADMIN) ?? false;

export const toMemberRole = (role: unknown): MemberRole => {
  if (z.enum(MemberRole).safeParse(role).success) {
    return z.enum(MemberRole).parse(role);
  }

  return MemberRole.MEMBER;
};
