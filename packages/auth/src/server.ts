import { drizzleAdapter } from "@better-auth/drizzle-adapter";
import { i18n, locales, TranslationDictionary } from "@better-auth/i18n";
import { passkey } from "@better-auth/passkey";
import { APIError, createAuthMiddleware } from "better-auth/api";
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
import { syncSubscriptionSeats } from "./lib/server";
import { getUrl, trustedOrigins } from "./lib/utils";
import { ac, roles } from "./rbac";
import { AuthProvider, SocialProvider, VerificationType } from "./types";

export const auth = betterAuth({
  appName: "TurboStarter",
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
    }),
    after: createAuthMiddleware(async (ctx) => {
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
      emailDomainName: "turbostarter.dev",
    }),
    admin(),
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
    cookiePrefix: "turbostarter",
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
