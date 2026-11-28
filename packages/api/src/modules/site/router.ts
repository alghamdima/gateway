import { Hono } from "hono";

import { auth, clientIp } from "@workspace/auth/server";
import { EmailTemplate } from "@workspace/email";
import { sendEmail } from "@workspace/email/server";
import { logger } from "@workspace/shared/logger";

import { validate } from "../../middleware";
import { contactRequestSchema } from "../../schema";
import { countContactAttempt, createContactRequest } from "./contact";
import { getPublicSiteSettings, getSiteSettings } from "./settings";

import type { ContactRequestInput } from "../../schema";

/**
 * Tells staff a message arrived, at the address they set (ADMIN-20). The
 * message is already saved, so a failure here is logged and nothing else:
 * staff find it in the admin either way.
 */
const notifyStaff = async (input: ContactRequestInput) => {
  try {
    const { contactEmail } = await getSiteSettings();
    if (!contactEmail) {
      return;
    }
    await sendEmail({
      to: contactEmail,
      template: EmailTemplate.CONTACT_FORM,
      variables: {
        topic: input.topic,
        name: input.name,
        email: input.email,
        ...(input.company ? { company: input.company } : {}),
        message: input.message,
      },
    });
  } catch (error) {
    logger.error(error);
  }
};

/**
 * The public site's own routes, open to visitors: which parts staff have
 * switched on (SITE-01), and the contact page's form (SITE-02).
 */
export const siteRouter = new Hono()
  .get("/settings", async (c) => c.json(await getPublicSiteSettings()))
  .post("/contact", validate("json", contactRequestSchema), async (c) => {
    const input = c.req.valid("json");
    await countContactAttempt(
      clientIp(c.req.raw.headers) ?? input.email.toLowerCase(),
    );
    const session = await auth.api.getSession({ headers: c.req.raw.headers });
    const { id } = await createContactRequest({
      ...input,
      userId: session?.user.id ?? null,
    });
    await notifyStaff(input);
    return c.json({ id });
  });
