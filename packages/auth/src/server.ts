import { drizzleAdapter } from "@better-auth/drizzle-adapter";
import { i18n, locales, TranslationDictionary } from "@better-auth/i18n";
import { passkey } from "@better-auth/passkey";
import {
  APIError,
  createAuthMiddleware,
  getSessionFromCtx,
  isAPIError,
} from "better-auth/api";
import { deleteSessionCookie } from "better-auth/cookies";
import { betterAuth } from "better-auth/minimal";
import { nextCookies } from "better-auth/next-js";
import {
  anonymous,
  emailOTP,
  magicLink,
  twoFactor,
  organization,
  admin,
  oneTap,
  lastLoginMethod,
} from "better-auth/plugins";

import * as schema from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { EmailTemplate } from "@workspace/email";
import { sendEmail } from "@workspace/email/server";
import { config, Locale } from "@workspace/i18n";
import { getLocaleFromRequest } from "@workspace/i18n/server";
import { logger } from "@workspace/shared/logger";

import { env } from "./env";
import { hooks } from "./hooks";
import { AUTH_ADMIN_ACTIONS, clientIp, recordStaffAction } from "./lib/audit";
import { syncSubscriptionSeats } from "./lib/server";
import { getUrl, trustedOrigins } from "./lib/utils";
import {
  ac,
  isStaff,
  roles,
  STAFF_ROLE_NAMES,
  staffAc,
  staffRoles,
} from "./rbac";
import { AuthProvider, SocialProvider, VerificationType } from "./types";

/**
 * Where a staff member with two-factor authentication may get a session.
 * Better Auth asks for the second factor only on password sign-ins; every
 * other way in (magic link, email code, Google, GitHub, Apple, one-tap) would
 * give staff a full session on their mailbox or social account alone
 * (ADR 0011).
 */
const STAFF_SESSION_PATHS = new Set([
  // the password step: the two-factor plugin turns it into a challenge
  "/sign-in/email",
  "/two-factor/verify-totp",
  "/two-factor/verify-backup-code",
  // a passkey is itself a strong second factor
  "/passkey/verify-authentication",
  "/sign-in/passkey",
  // already signed in with both factors
  "/change-password",
  "/admin/impersonate-user",
  "/admin/stop-impersonating",
]);

const refuseStaffSessionsWithoutSecondFactor = async (
  ctx: Parameters<Parameters<typeof createAuthMiddleware>[0]>[0],
) => {
  const created = ctx.context.newSession;
  if (!created || STAFF_SESSION_PATHS.has(ctx.path)) {
    return;
  }
  // the hook context types the user loosely; read the two fields we need
  const role = typeof created.user.role === "string" ? created.user.role : null;
  if (!isStaff({ role }) || created.user.twoFactorEnabled !== true) {
    return;
  }
  await ctx.context.internalAdapter.deleteSession(created.session.token);
  deleteSessionCookie(ctx);
  throw new APIError("FORBIDDEN", {
    message:
      "Staff accounts sign in with their password and authenticator app, or a passkey",
  });
};

export const auth = betterAuth({
  appName: "Lemru",
  // No cookie cache: every request reads the session from the database, so a
  // ban, a revoked session or a changed role applies on the next request
  // instead of up to five minutes later.
  session: {},
  rateLimit: {
    // on in every environment, so it is exercised before production; counters
    // are shared by every instance
    enabled: true,
    storage: "database",
    customRules: {
      "/sign-in/email": { window: 60, max: 5 },
      "/sign-up/email": { window: 60, max: 3 },
      "/request-password-reset": { window: 60, max: 3 },
      "/sign-in/magic-link": { window: 60, max: 3 },
      "/email-otp/send-verification-otp": { window: 60, max: 3 },
      "/two-factor/verify-totp": { window: 60, max: 5 },
      "/two-factor/verify-backup-code": { window: 60, max: 5 },
    },
  },
  user: {
    deleteUser: {
      enabled: true,
      sendDeleteAccountVerification: async ({ user, url }, request) =>
        sendEmail({
          to: user.email,
          template: EmailTemplate.DELETE_ACCOUNT,
          locale: getLocaleFromRequest(request),
          variables: {
            url: getUrl({
              request,
              url,
              type: VerificationType.DELETE_ACCOUNT,
            }).toString(),
          },
        }),
    },
    changeEmail: {
      enabled: true,
      updateEmailWithoutVerification: false,
      sendChangeEmailConfirmation: async ({ user, newEmail, url }, request) =>
        sendEmail({
          to: user.email,
          template: EmailTemplate.CHANGE_EMAIL,
          locale: getLocaleFromRequest(request),
          variables: {
            url: getUrl({
              request,
              url,
              type: VerificationType.CONFIRM_EMAIL,
            }).toString(),
            newEmail,
          },
        }),
    },
  },
  trustedOrigins,
  emailAndPassword: {
    enabled: true,
    requireEmailVerification: true,
    revokeSessionsOnPasswordReset: true,
    sendResetPassword: async ({ user, url }, request) =>
      sendEmail({
        to: user.email,
        template: EmailTemplate.RESET_PASSWORD,
        locale: getLocaleFromRequest(request),
        variables: {
          url,
        },
      }),
  },
  emailVerification: {
    sendOnSignUp: true,
    autoSignInAfterVerification: true,
    sendVerificationEmail: async ({ user, url }, request) =>
      sendEmail({
        to: user.email,
        template: EmailTemplate.CONFIRM_EMAIL,
        locale: getLocaleFromRequest(request),
        variables: {
          url: getUrl({
            request,
            url,
            type: VerificationType.CONFIRM_EMAIL,
          }).toString(),
        },
      }),
  },
  database: drizzleAdapter(db, {
    provider: "pg",
    schema,
  }),
  databaseHooks: {
    user: hooks.user,
  },
  hooks: {
    before: createAuthMiddleware(async (ctx) => {
      // Anonymous accounts are off for a paid API: they would let anyone
      // create accounts without an email. The plugin stays registered only so
      // the existing schema and types keep working; its sign-in is refused.
      if (ctx.path === "/sign-in/anonymous") {
        throw new APIError("FORBIDDEN", {
          message: "Anonymous sign-in is disabled",
        });
      }

      // Staff act on every customer's account and money (ADR 0011): their
      // admin actions need two-factor authentication turned on. Stopping an
      // impersonation runs as the customer, who is not staff, so it passes.
      if (ctx.path.startsWith("/admin/")) {
        const session = await getSessionFromCtx(ctx);
        // the hook context types the user loosely; read the two fields we need
        const role =
          typeof session?.user.role === "string" ? session.user.role : null;
        if (
          session &&
          isStaff({ role }) &&
          session.user.twoFactorEnabled !== true
        ) {
          throw new APIError("FORBIDDEN", {
            message: "Turn on two-factor authentication to use admin actions",
          });
        }
      }
    }),
    after: createAuthMiddleware(async (ctx) => {
      await refuseStaffSessionsWithoutSecondFactor(ctx);
      await recordAuthAdminAction(ctx);

      if (ctx.path !== "/organization/leave") {
        return;
      }

      const organizationId = ctx.body?.organizationId;
      if (typeof organizationId === "string") {
        await syncSubscriptionSeats(organizationId);
      }
    }),
  },
  plugins: [
    i18n({
      detection: ["cookie", "header"],
      translations: {
        en: locales.en,
        es: locales.es,
      } satisfies Record<Locale, TranslationDictionary>,
      localeCookie: config.cookie,
      defaultLocale: config.defaultLocale,
    }),
    magicLink({
      sendMagicLink: async ({ email, url }, ctx) =>
        sendEmail({
          to: email,
          template: EmailTemplate.MAGIC_LINK,
          locale: getLocaleFromRequest(ctx?.request),
          variables: {
            url: getUrl({
              request: ctx?.request,
              url,
              type: VerificationType.MAGIC_LINK,
            }).toString(),
          },
        }),
    }),
    emailOTP({
      async sendVerificationOTP({ email, otp, type }, ctx) {
        if (type !== "sign-in") {
          /* Handle other types if you want to use OTP verification
          for other purposes (e.g. change email, delete account, etc.) */
          return;
        }

        return sendEmail({
          to: email,
          template: EmailTemplate.SIGN_IN_OTP,
          locale: getLocaleFromRequest(ctx?.request),
          variables: { otp, url: getUrl({ request: ctx?.request }).toString() },
        });
      },
    }),
    passkey(),
    twoFactor(),
    anonymous({
      emailDomainName: "anonymous.invalid",
    }),
    admin({
      ac: staffAc,
      roles: staffRoles,
      // impersonating any staff member needs "impersonate-admins", which no role has
      adminRoles: [...STAFF_ROLE_NAMES],
    }),
    organization({
      requireEmailVerificationOnInvitation: true,
      sendInvitationEmail: async (
        { invitation, inviter, organization },
        request,
      ) => {
        const url = getUrl({
          request,
        });
        url.searchParams.set("invitationId", invitation.id);
        url.searchParams.set("email", invitation.email);

        return sendEmail({
          to: invitation.email,
          template: EmailTemplate.ORGANIZATION_INVITATION,
          locale: getLocaleFromRequest(request),
          variables: {
            url: url.toString(),
            inviter: inviter.user.name,
            organization: organization.name,
          },
        });
      },
      ac,
      roles,
      organizationHooks: hooks.organization,
    }),
    lastLoginMethod({
      customResolveMethod: (ctx) => {
        switch (ctx.path) {
          case "/magic-link/verify":
            return AuthProvider.MAGIC_LINK;
          case "/passkey/verify-authentication":
            return AuthProvider.PASSKEY;
          case "/sign-in/email-otp":
            return AuthProvider.EMAIL_OTP;
          default:
            return null;
        }
      },
    }),
    oneTap(),
    nextCookies(),
  ],
  socialProviders: {
    [SocialProvider.APPLE]: {
      clientId: env.APPLE_CLIENT_ID,
      clientSecret: env.APPLE_CLIENT_SECRET,
      appBundleIdentifier: env.APPLE_APP_BUNDLE_IDENTIFIER,
    },
    [SocialProvider.GOOGLE]: {
      clientId: env.GOOGLE_CLIENT_ID,
      clientSecret: env.GOOGLE_CLIENT_SECRET,
    },
    [SocialProvider.GITHUB]: {
      clientId: env.GITHUB_CLIENT_ID,
      clientSecret: env.GITHUB_CLIENT_SECRET,
    },
  },
  advanced: {
    cookiePrefix: "lemru",
    database: {
      joins: true,
    },
    cookies: {
      state: {
        attributes: {
          sameSite: "none",
          secure: true,
        },
      },
    },
  },
  logger: {
    log: (level, ...args) => logger[level](...args),
  },
});

/**
 * Records a successful Better Auth admin action in the audit log
 * (ADR 0011). The request still carries the staff member's own session, so
 * they are the actor, including for impersonate-user; for
 * stop-impersonating the session is the customer's and names the staff
 * member in impersonatedBy.
 */
async function recordAuthAdminAction(
  ctx: Parameters<Parameters<typeof createAuthMiddleware>[0]>[0],
) {
  const entry = AUTH_ADMIN_ACTIONS[ctx.path];
  if (!entry || isAPIError(ctx.context.returned)) {
    return;
  }
  const session = await getSessionFromCtx(ctx);
  if (!session) {
    return;
  }
  const impersonatedBy =
    typeof session.session.impersonatedBy === "string"
      ? session.session.impersonatedBy
      : null;
  const body: Record<string, unknown> =
    ctx.body && typeof ctx.body === "object" ? ctx.body : {};
  const targetId =
    typeof body.userId === "string"
      ? body.userId
      : impersonatedBy
        ? session.user.id
        : undefined;

  await recordStaffAction({
    actorId: impersonatedBy ?? session.user.id,
    actorRole:
      impersonatedBy === null && typeof session.user.role === "string"
        ? session.user.role
        : null,
    action: entry.action,
    targetType: "user",
    targetId,
    details: entry.details?.(body),
    ipAddress: clientIp(ctx.request?.headers),
  });
}

export type AuthErrorCode = keyof typeof auth.$ERROR_CODES;
export type Session = typeof auth.$Infer.Session;
export type User = Session["user"];
export type Invitation = typeof auth.$Infer.Invitation;
export type Organization = typeof auth.$Infer.Organization;
export type ActiveOrganization = typeof auth.$Infer.ActiveOrganization;
export type Member = typeof auth.$Infer.Member;
export type Permissions = NonNullable<
  NonNullable<
    Parameters<typeof auth.api.hasPermission>[0]
  >["body"]["permissions"]
>;

export * from "./hooks";
export * from "./lib/server";
export * from "./lib/audit";
