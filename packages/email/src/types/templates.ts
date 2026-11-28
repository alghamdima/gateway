export interface CommonEmailProps {
  readonly locale?: string;
}

const AuthEmailTemplate = {
  RESET_PASSWORD: "reset-password",
  MAGIC_LINK: "magic-link",
  SIGN_IN_OTP: "sign-in-otp",
  CONFIRM_EMAIL: "confirm-email",
  DELETE_ACCOUNT: "delete-account",
  CHANGE_EMAIL: "change-email",
  ORGANIZATION_INVITATION: "organization-invitation",
} as const;

type AuthEmailTemplate =
  (typeof AuthEmailTemplate)[keyof typeof AuthEmailTemplate];

type AuthEmailVariables = Record<
  Exclude<
    AuthEmailTemplate,
    | typeof AuthEmailTemplate.ORGANIZATION_INVITATION
    | typeof AuthEmailTemplate.SIGN_IN_OTP
  >,
  { url: string }
> & {
  [AuthEmailTemplate.SIGN_IN_OTP]: { url: string; otp: string };
  [AuthEmailTemplate.CHANGE_EMAIL]: {
    url: string;
    newEmail: string;
  };
  [AuthEmailTemplate.ORGANIZATION_INVITATION]: {
    url: string;
    inviter: string;
    organization: string;
  };
};

export const EmailTemplate = {
  ...AuthEmailTemplate,
  CONTACT_FORM: "contact-form",
  RECONCILIATION_ALERT: "reconciliation-alert",
  LOW_BALANCE: "low-balance",
} as const;

export type EmailTemplate = (typeof EmailTemplate)[keyof typeof EmailTemplate];

export type EmailVariables = AuthEmailVariables & {
  [EmailTemplate.CONTACT_FORM]: {
    topic: string;
    name: string;
    email: string;
    company?: string;
    message: string;
  };
  [EmailTemplate.RECONCILIATION_ALERT]: {
    url: string;
    /** The UTC day checked, YYYY-MM-DD. */
    period: string;
    status: "mismatch" | "error";
    organizations: number;
    reapedHolds: number;
    unattributed: number;
  };
  [EmailTemplate.LOW_BALANCE]: {
    url: string;
    organization: string;
    /** Formatted amounts, e.g. "$1.20". */
    available: string;
    threshold: string;
  };
};
