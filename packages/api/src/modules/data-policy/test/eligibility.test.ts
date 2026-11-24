import { describe, expect, it } from "vitest";

import { accessFor, isOpen, OPEN_POLICY, satisfies } from "../eligibility";

import type { ProviderCatalogEntry } from "../../admin/providers/providers";
import type { DataPolicy } from "../eligibility";

const entry = (
  keyId: string,
  provider: string,
  terms: {
    region: "sa" | "eu" | "us" | "other";
    retentionDays: number | null;
    trainsOnData: boolean | null;
  } | null,
  isInGateway = true,
): ProviderCatalogEntry => ({
  keyId,
  name: keyId,
  provider,
  isEnabled: true,
  isInGateway,
  terms: terms && {
    ...terms,
    agreement: "direct",
    source: "contract",
    verifiedOn: "2026-11-24",
    updatedBy: "staff",
    updatedAt: new Date(),
  },
  isZeroRetention: terms?.retentionDays === 0,
  isNeverTrainedOn: terms?.trainsOnData === false,
});

const CATALOG = [
  entry("riyadh", "humain", {
    region: "sa",
    retentionDays: 0,
    trainsOnData: false,
  }),
  entry("jeddah", "humain", {
    region: "sa",
    retentionDays: 30,
    trainsOnData: false,
  }),
  entry("frankfurt", "azure", {
    region: "eu",
    retentionDays: 0,
    trainsOnData: false,
  }),
  entry("virginia", "openai", {
    region: "us",
    retentionDays: 30,
    trainsOnData: true,
  }),
  entry("half-known", "openai", {
    region: "us",
    retentionDays: null,
    trainsOnData: null,
  }),
  entry("nothing-recorded", "groq", null),
];

const policy = (change: Partial<DataPolicy>): DataPolicy => ({
  ...OPEN_POLICY,
  ...change,
});

const allowed = (p: DataPolicy) =>
  CATALOG.filter((e) => satisfies(e, p)).map((e) => e.keyId);

describe("data policy eligibility", () => {
  it("an open policy allows every endpoint, known or not, and reaches everything", () => {
    expect(isOpen(OPEN_POLICY)).toBe(true);
    expect(allowed(OPEN_POLICY)).toHaveLength(CATALOG.length);
    expect(accessFor(OPEN_POLICY, CATALOG)).toEqual({ allowAll: true });
  });

  const cases: [string, Partial<DataPolicy>, string[]][] = [
    ["zero retention", { requireZeroRetention: true }, ["riyadh", "frankfurt"]],
    [
      "no training",
      { forbidTraining: true },
      ["riyadh", "jeddah", "frankfurt"],
    ],
    ["in the Kingdom", { allowedRegions: ["sa"] }, ["riyadh", "jeddah"]],
    [
      "the Kingdom or the EU",
      { allowedRegions: ["sa", "eu"] },
      ["riyadh", "jeddah", "frankfurt"],
    ],
    [
      "one provider",
      { allowedProviders: ["openai"] },
      ["virginia", "half-known"],
    ],
    [
      "in the Kingdom with zero retention and no training",
      {
        allowedRegions: ["sa"],
        requireZeroRetention: true,
        forbidTraining: true,
      },
      ["riyadh"],
    ],
    [
      "a provider outside the allowed region",
      { allowedRegions: ["sa"], allowedProviders: ["azure"] },
      [],
    ],
  ];

  it.each(cases)(
    "%s allows exactly the endpoints with those terms",
    (_name, change, expected) => {
      const p = policy(change);
      expect(isOpen(p)).toBe(false);
      expect(allowed(p)).toEqual(expected);
    },
  );

  it("never counts unknown terms: no region, kept, trained on", () => {
    for (const p of [
      policy({ requireZeroRetention: true }),
      policy({ forbidTraining: true }),
      policy({ allowedRegions: ["sa", "eu", "us", "other"] }),
    ]) {
      expect(allowed(p)).not.toContain("nothing-recorded");
    }
    expect(allowed(policy({ requireZeroRetention: true }))).not.toContain(
      "half-known",
    );
    expect(allowed(policy({ forbidTraining: true }))).not.toContain(
      "half-known",
    );
  });

  it("leaves out terms whose key the gateway no longer has", () => {
    const gone = entry(
      "gone",
      "humain",
      { region: "sa", retentionDays: 0, trainsOnData: false },
      false,
    );
    expect(satisfies(gone, policy({ allowedRegions: ["sa"] }))).toBe(false);
    expect(satisfies(gone, OPEN_POLICY)).toBe(false);
  });

  it("names the allowed provider keys per provider for the gateway", () => {
    expect(accessFor(policy({ forbidTraining: true }), CATALOG)).toEqual({
      allowAll: false,
      providers: [
        { provider: "humain", keyIds: ["riyadh", "jeddah"] },
        { provider: "azure", keyIds: ["frankfurt"] },
      ],
    });
  });

  it("reaches nothing when no endpoint satisfies the policy, rather than everything", () => {
    expect(
      accessFor(
        policy({ allowedRegions: ["sa"], allowedProviders: ["azure"] }),
        CATALOG,
      ),
    ).toEqual({ allowAll: false, providers: [] });
  });
});
