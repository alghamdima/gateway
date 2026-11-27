import { Stripe } from "stripe";

import { secretKeyFor } from "./config";
import { env } from "./env";

import type { PaymentMode } from "../types";

const clients = new Map<string, Stripe>();

/**
 * The Stripe client of a mode, with that mode's key (./config.ts). Top-ups
 * always name the mode staff chose. Without a mode it is the template's
 * subscription code, which keeps using STRIPE_SECRET_KEY as before (BILL-11
 * removes it).
 */
export const stripe = (mode?: PaymentMode) => {
  const key = mode ? secretKeyFor(mode) : (env.STRIPE_SECRET_KEY ?? null);
  if (!key) {
    throw new Error(
      mode
        ? `Stripe has no ${mode} secret key of that mode`
        : "STRIPE_SECRET_KEY is not set",
    );
  }

  let client = clients.get(key);
  if (!client) {
    client = new Stripe(key);
    clients.set(key, client);
  }
  return client;
};
