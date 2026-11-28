import { createPersonalAccount } from "../../lib/personal";
import { generateName } from "../../lib/utils";
import { DatabaseHooks } from "../types";

export const create = {
  before: async (user) => {
    const name = user.name?.trim() || generateName(user.email);

    return {
      data: {
        ...user,
        name,
      },
    };
  },
  // every user has a personal account from the start (ADR 0020), whichever
  // way they signed up, an invitation included
  after: async (user) => {
    await createPersonalAccount(user.id);
  },
} satisfies NonNullable<DatabaseHooks["user"]>["create"];
