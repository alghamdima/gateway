import { Hono } from "hono";
import { beforeEach, describe, expect, it, vi } from "vitest";

interface FakeSession {
  user: { id: string };
  session: { id: string; impersonatedBy?: string };
}
const getSession = vi.fn<() => Promise<FakeSession | null>>();
vi.mock("@workspace/auth/server", () => ({ auth: { api: { getSession } } }));

const { refuseImpersonatedWrites } = await import("../impersonation");
const { getStatusCode } = await import("@workspace/shared/utils");

const app = new Hono()
  .basePath("/api")
  .use(refuseImpersonatedWrites)
  .all("*", (c) => c.text("reached"))
  .onError(
    (error) => new Response("refused", { status: getStatusCode(error) }),
  );

const impersonated = {
  user: { id: "customer" },
  session: { id: "s1", impersonatedBy: "staff" },
};
const own = { user: { id: "customer" }, session: { id: "s2" } };

describe("refuseImpersonatedWrites", () => {
  beforeEach(() => getSession.mockReset());

  it.each([
    ["POST", "/api/keys/org_1"],
    ["PATCH", "/api/keys/org_1/key_1"],
    ["DELETE", "/api/keys/org_1/key_1"],
    ["PUT", "/api/credits/org_1/low-balance-alert"],
    ["POST", "/api/billing/checkout"],
    ["POST", "/api/auth/passkey/generate-register-options"],
    ["POST", "/api/auth/organization/invite-member"],
    ["POST", "/api/auth/change-password"],
  ])("refuses %s %s while impersonating", async (method, path) => {
    getSession.mockResolvedValue(impersonated);
    const res = await app.request(path, { method });
    expect(res.status).toBe(403);
    expect(await res.text()).not.toContain("reached");
  });

  it.each(["/api/auth/admin/stop-impersonating", "/api/auth/sign-out"])(
    "lets the impersonation end through %s",
    async (path) => {
      getSession.mockResolvedValue(impersonated);
      const res = await app.request(path, { method: "POST" });
      expect(await res.text()).toBe("reached");
    },
  );

  it("lets an impersonated session read", async () => {
    getSession.mockResolvedValue(impersonated);
    const res = await app.request("/api/keys/org_1");
    expect(await res.text()).toBe("reached");
    expect(getSession).not.toHaveBeenCalled();
  });

  it.each([
    ["the user's own session", own],
    ["no session (webhooks, schedulers, API keys)", null],
  ])("lets writes through for %s", async (_, session) => {
    getSession.mockResolvedValue(session);
    const res = await app.request("/api/keys/org_1", { method: "POST" });
    expect(await res.text()).toBe("reached");
  });
});
