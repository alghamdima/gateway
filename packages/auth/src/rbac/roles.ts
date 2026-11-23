import {
  adminAc,
  memberAc,
  ownerAc,
} from "better-auth/plugins/organization/access";

import { ac } from "./permissions";

const member = ac.newRole({
  billing: ["read"],
  apiKey: ["read"],
  ...memberAc.statements,
});

const admin = ac.newRole({
  billing: ["create", "read"],
  apiKey: ["create", "read", "update", "delete"],
  ...adminAc.statements,
});

const owner = ac.newRole({
  billing: ["create", "read", "update", "delete"],
  apiKey: ["create", "read", "update", "delete"],
  ...ownerAc.statements,
});

export const roles = {
  member,
  admin,
  owner,
} as const;
