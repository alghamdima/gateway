import { and, eq, isNull, sql } from "@workspace/db";
import { modelPricePush } from "@workspace/db/schema";
import { db } from "@workspace/db/server";
import { GatewayError } from "@workspace/gateway-client/server";
import { logger } from "@workspace/shared/logger";

import { getVersionsInForce, targetKey } from "./prices";
import { ratesByField, toPatch } from "./rates";

import type { ModelPriceVersion, PriceTarget } from "./prices";
import type {
  GatewayClient,
  GatewayPricingOverride,
  PricingOverrideInput,
} from "@workspace/gateway-client/server";

/**
 * Our prices reach the gateway as its own pricing overrides
 * (docs/platform/adr/0018-model-price-table.md): the version in force for each
 * target becomes one override, scope provider or provider_key, exact model,
 * every rate in the patch, named after the version so each usage entry can
 * name the price it was charged at. The push is idempotent and the
 * reconciliation says where the gateway differs from us.
 */

/** Every override the control plane writes is named so; the rest of the name is the version id. */
export const OVERRIDE_NAME_PREFIX = "lemru-price:";

/**
 * The request types the credits plugin charges (plugins/credits/plugin.go,
 * `classify`); a type joins here when it is switched on.
 */
export const PUSHED_REQUEST_TYPES = [
  "chat_completion",
  "text_completion",
  "responses",
  "embedding",
] as const;

export const overrideName = (versionId: number) =>
  `${OVERRIDE_NAME_PREFIX}${versionId}`;

/** The version id an override name carries, or undefined when it is not one of ours. */
export const versionIdOf = (name: string): number | undefined => {
  if (!name.startsWith(OVERRIDE_NAME_PREFIX)) {
    return undefined;
  }
  const id = Number(name.slice(OVERRIDE_NAME_PREFIX.length));
  return Number.isSafeInteger(id) && id > 0 ? id : undefined;
};

/** The override that carries a version: exactly what the gateway is sent. */
export const overrideFor = (
  version: ModelPriceVersion,
): PricingOverrideInput => ({
  name: overrideName(version.id),
  scope:
    version.providerKeyId === null
      ? { kind: "provider", provider: version.provider }
      : { kind: "provider_key", providerKeyId: version.providerKeyId },
  model: version.model,
  requestTypes: PUSHED_REQUEST_TYPES,
  patch: toPatch(version),
});

/** Whether an override covers this target (whoever wrote it). */
const coversTarget = (override: GatewayPricingOverride, target: PriceTarget) =>
  override.matchType === "exact" &&
  override.pattern.toLowerCase() === target.model.toLowerCase() &&
  (target.providerKeyId === null
    ? override.scopeKind === "provider" &&
      override.providerId === target.provider
    : override.scopeKind === "provider_key" &&
      override.providerKeyId === target.providerKeyId);

type PushState = typeof modelPricePush.$inferSelect;

const stateTargetIs = ({ provider, model, providerKeyId }: PriceTarget) =>
  and(
    eq(modelPricePush.provider, provider),
    eq(modelPricePush.model, model),
    providerKeyId === null
      ? isNull(modelPricePush.providerKeyId)
      : eq(modelPricePush.providerKeyId, providerKeyId),
  );

const getState = async (target: PriceTarget) => {
  const [row] = await db
    .select()
    .from(modelPricePush)
    .where(stateTargetIs(target));
  return row;
};

/** One short statement per outcome: never a transaction held across a gateway call. */
const saveState = async (
  target: PriceTarget,
  values: Partial<
    Omit<PushState, "id" | "provider" | "model" | "providerKeyId">
  >,
) => {
  await db
    .insert(modelPricePush)
    .values({
      provider: target.provider,
      model: target.model,
      providerKeyId: target.providerKeyId,
      attemptedAt: new Date(),
      ...values,
    })
    .onConflictDoUpdate({
      target: [
        modelPricePush.provider,
        modelPricePush.model,
        modelPricePush.providerKeyId,
      ],
      set: { attemptedAt: new Date(), ...values },
    });
};

export type PushOutcome =
  | { status: "unchanged" }
  | { status: "pushed"; overrideId: string }
  | { status: "removed" }
  | { status: "failed"; error: string };

export interface PushOptions {
  /** Push even when the state says the gateway has this version (after drift). */
  force?: boolean;
  /** The gateway's overrides, read once for a batch, to adopt one of ours the state lost. */
  overrides?: GatewayPricingOverride[];
}

/**
 * Makes the gateway carry `version` for its target, or nothing when `version`
 * is undefined or a removal. Idempotent: a target already carrying the version
 * is left alone; an override the gateway lost is created again; before
 * creating one, an override of ours for the same target is adopted, so a lost
 * state row never makes a duplicate.
 */
export const pushTarget = async (
  gateway: GatewayClient,
  target: PriceTarget,
  version: ModelPriceVersion | undefined,
  { force = false, overrides }: PushOptions = {},
): Promise<PushOutcome> => {
  const state = await getState(target);
  try {
    if (!version || version.isRemoved) {
      if (state?.gatewayOverrideId) {
        await gateway.deletePricingOverride(state.gatewayOverrideId);
      }
      await saveState(target, {
        gatewayOverrideId: null,
        pushedPriceId: null,
        pushedAt: new Date(),
        lastError: null,
      });
      return { status: "removed" };
    }
    if (
      !force &&
      state?.gatewayOverrideId &&
      state.pushedPriceId === version.id &&
      !state.lastError
    ) {
      return { status: "unchanged" };
    }

    const input = overrideFor(version);
    let overrideId = state?.gatewayOverrideId ?? undefined;
    if (!overrideId) {
      const known = overrides ?? (await gateway.listPricingOverrides());
      overrideId = known.find(
        (override) =>
          versionIdOf(override.name) !== undefined &&
          coversTarget(override, target),
      )?.id;
    }
    let saved: GatewayPricingOverride | undefined;
    if (overrideId) {
      try {
        saved = await gateway.updatePricingOverride(overrideId, input);
      } catch (error) {
        if (!(error instanceof GatewayError && error.status === 404)) {
          throw error;
        }
      }
    }
    saved ??= await gateway.createPricingOverride(input);
    await saveState(target, {
      gatewayOverrideId: saved.id,
      pushedPriceId: version.id,
      pushedAt: new Date(),
      lastError: null,
    });
    return { status: "pushed", overrideId: saved.id };
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    logger.error(
      `model-prices: push of ${target.provider}/${target.model}${target.providerKeyId ? ` (key ${target.providerKeyId})` : ""} failed: ${message}`,
    );
    await saveState(target, { lastError: message.slice(0, 2000) });
    return { status: "failed", error: message };
  }
};

export interface PushSummary {
  pushed: number;
  removed: number;
  unchanged: number;
  failed: number;
}

const PUSH_CONCURRENCY = 8;

/**
 * Brings every target in line with the version in force at `now`: versions
 * whose time has come (a future price starts here), changes the gateway has
 * not received, failed pushes, and keys whose own price ended.
 */
export const pushDuePrices = async (
  gateway: GatewayClient,
  now = new Date(),
  { force = false, only }: { force?: boolean; only?: Set<string> } = {},
): Promise<PushSummary> => {
  const [versions, states] = await Promise.all([
    getVersionsInForce(now, {}, { includeRemoved: true }),
    db.select().from(modelPricePush),
  ]);
  const stateByTarget = new Map(
    states.map((state) => [targetKey(state), state]),
  );
  const work: {
    target: PriceTarget;
    version: ModelPriceVersion | undefined;
  }[] = [];
  const seen = new Set<string>();
  for (const version of versions) {
    const key = targetKey(version);
    seen.add(key);
    if (only && !only.has(key)) {
      continue;
    }
    const state = stateByTarget.get(key);
    const carries = version.isRemoved
      ? !state?.gatewayOverrideId
      : state?.pushedPriceId === version.id &&
        Boolean(state.gatewayOverrideId) &&
        !state.lastError;
    if (force || !carries) {
      work.push({ target: version, version });
    }
  }
  // an override of ours for a target that has no version at all (none should exist)
  for (const state of states) {
    if (!seen.has(targetKey(state)) && state.gatewayOverrideId) {
      work.push({ target: state, version: undefined });
    }
  }

  const summary: PushSummary = {
    pushed: 0,
    removed: 0,
    unchanged: 0,
    failed: 0,
  };
  if (work.length === 0) {
    return summary;
  }
  const overrides = await gateway.listPricingOverrides();
  for (let index = 0; index < work.length; index += PUSH_CONCURRENCY) {
    const outcomes = await Promise.all(
      work
        .slice(index, index + PUSH_CONCURRENCY)
        .map(({ target, version }) =>
          pushTarget(gateway, target, version, { force, overrides }),
        ),
    );
    for (const outcome of outcomes) {
      summary[outcome.status] += 1;
    }
  }
  return summary;
};

/** Pushes one target's version in force now: after staff save or approve a price. */
export const pushNow = async (
  gateway: GatewayClient,
  target: PriceTarget,
  now = new Date(),
) => {
  const summary = await pushDuePrices(gateway, now, {
    only: new Set([targetKey(target)]),
  });
  return summary;
};

export const DRIFT_KINDS = [
  "missing",
  "wrong_target",
  "wrong_request_types",
  "wrong_rates",
  "stale_version",
  "not_removed",
  "orphan",
  "foreign_override",
  "effective_price",
  "not_in_catalog",
] as const;
export type DriftKind = (typeof DRIFT_KINDS)[number];

export interface Drift {
  kind: DriftKind;
  provider: string;
  model: string;
  providerKeyId: string | null;
  detail: string;
}

export interface PriceCheck {
  checkedAt: Date;
  /** Targets with a price in force. */
  targets: number;
  drift: Drift[];
}

const sameRequestTypes = (types: string[]) =>
  types.length === PUSHED_REQUEST_TYPES.length &&
  PUSHED_REQUEST_TYPES.every((type) => types.includes(type));

/** Fields whose float differs between what we price and what the gateway holds. */
const differingFields = (
  expected: Record<string, number>,
  actual: Record<string, number>,
) =>
  [...new Set([...Object.keys(expected), ...Object.keys(actual)])]
    .filter((field) => expected[field] !== actual[field])
    .sort();

const DISPLAYED_FIELDS = [
  "input_cost_per_token",
  "output_cost_per_token",
  "cache_read_input_token_cost",
  "cache_creation_input_token_cost",
] as const;

/**
 * Compares the gateway with our prices in force at `now`: each override we
 * push (exists, right target, request types, every rate, named after the
 * version in force), keys whose price ended, overrides of ours we no longer
 * know, other overrides that could win over ours, and the price the gateway's
 * catalog says it charges per provider and model.
 */
export const checkModelPrices = async (
  gateway: GatewayClient,
  now = new Date(),
): Promise<PriceCheck> => {
  const [versions, overrides, catalog] = await Promise.all([
    getVersionsInForce(now, {}, { includeRemoved: true }),
    gateway.listPricingOverrides(),
    gateway.listModels(),
  ]);
  const drift: Drift[] = [];
  const report = (target: PriceTarget, kind: DriftKind, detail: string) =>
    drift.push({
      kind,
      provider: target.provider,
      model: target.model,
      providerKeyId: target.providerKeyId,
      detail,
    });

  const ours = overrides.filter(
    (override) => versionIdOf(override.name) !== undefined,
  );
  const matched = new Set<string>();
  const priced = versions.filter((version) => !version.isRemoved);

  for (const version of versions) {
    const candidates = ours.filter((override) =>
      coversTarget(override, version),
    );
    if (version.isRemoved) {
      for (const override of candidates) {
        matched.add(override.id);
        report(
          version,
          "not_removed",
          `override ${override.id} still prices this key`,
        );
      }
      continue;
    }
    const override =
      candidates.find((item) => versionIdOf(item.name) === version.id) ??
      candidates[0];
    if (!override) {
      // an override of ours carrying this version under another target is a wrong target
      const misplaced = ours.find(
        (item) => versionIdOf(item.name) === version.id,
      );
      if (misplaced) {
        matched.add(misplaced.id);
        report(
          version,
          "wrong_target",
          `override ${misplaced.id} has scope ${misplaced.scopeKind} and pattern ${misplaced.pattern}`,
        );
      } else {
        report(
          version,
          "missing",
          "the gateway has no override for this price",
        );
      }
      continue;
    }
    for (const extra of candidates) {
      matched.add(extra.id);
      if (extra.id !== override.id) {
        report(
          version,
          "orphan",
          `a second override ${extra.id} covers this target`,
        );
      }
    }
    if (versionIdOf(override.name) !== version.id) {
      report(
        version,
        "stale_version",
        `the gateway carries ${override.name}, the price in force is ${overrideName(version.id)}`,
      );
    }
    if (!sameRequestTypes(override.requestTypes)) {
      report(version, "wrong_request_types", override.requestTypes.join(", "));
    }
    const fields = differingFields(toPatch(version), override.patch);
    if (fields.length > 0) {
      report(version, "wrong_rates", fields.join(", "));
    }
  }

  for (const override of ours) {
    if (!matched.has(override.id)) {
      drift.push({
        kind: "orphan",
        provider: override.providerId ?? "",
        model: override.pattern,
        providerKeyId: override.providerKeyId,
        detail: `override ${override.id} (${override.name}) prices nothing we have`,
      });
    }
  }

  // someone else's override at a scope that applies to every request of a model we price
  const pricedModels = priced.filter(
    (version) => version.providerKeyId === null,
  );
  for (const override of overrides) {
    if (versionIdOf(override.name) !== undefined) {
      continue;
    }
    if (override.scopeKind !== "global" && override.scopeKind !== "provider") {
      continue;
    }
    const pattern = override.pattern.toLowerCase();
    for (const version of pricedModels) {
      const model = version.model.toLowerCase();
      const sameProvider =
        override.scopeKind === "global" ||
        override.providerId === version.provider;
      const matches =
        override.matchType === "wildcard"
          ? model.startsWith(pattern.replace(/\*$/, ""))
          : model === pattern;
      if (sameProvider && matches) {
        report(
          version,
          "foreign_override",
          `override ${override.id} (${override.name}, ${override.scopeKind}) also matches`,
        );
      }
    }
  }

  // what the gateway's catalog says it charges, provider-wide
  const catalogByModel = new Map(
    catalog.map((model) => [
      `${model.provider}/${model.name.toLowerCase()}`,
      model,
    ]),
  );
  for (const version of priced) {
    if (version.providerKeyId !== null) {
      continue;
    }
    const entry = catalogByModel.get(
      `${version.provider}/${version.model.toLowerCase()}`,
    );
    if (!entry) {
      report(
        version,
        "not_in_catalog",
        "the gateway does not list this model: it cannot be charged",
      );
      continue;
    }
    const ourRates = ratesByField(version);
    const differing = DISPLAYED_FIELDS.filter((field) => {
      const ours = ourRates[field];
      if (ours === undefined) {
        return false;
      }
      const effective = entry.overridden_pricing?.[field] ?? entry[field];
      return effective !== Number(ours);
    });
    if (differing.length > 0) {
      report(version, "effective_price", differing.join(", "));
    }
  }

  return { checkedAt: now, targets: priced.length, drift };
};

/** The drift a push can mend: our own overrides that are wrong, late, missing or left over. */
const REPAIRABLE: ReadonlySet<DriftKind> = new Set<DriftKind>([
  "missing",
  "wrong_target",
  "wrong_request_types",
  "wrong_rates",
  "stale_version",
  "not_removed",
]);

/**
 * The scheduled job: push what is due, compare with the gateway, push again
 * whatever drifted, and delete overrides of ours that price nothing.
 */
export const syncModelPrices = async (
  gateway: GatewayClient,
  now = new Date(),
) => {
  const pushed = await pushDuePrices(gateway, now);
  const check = await checkModelPrices(gateway, now);
  const drifted = new Set(
    check.drift
      .filter((item) => REPAIRABLE.has(item.kind))
      .map((item) => targetKey(item)),
  );
  const repaired = drifted.size
    ? await pushDuePrices(gateway, now, { force: true, only: drifted })
    : undefined;
  let orphansDeleted = 0;
  const overrides = check.drift.some((item) => item.kind === "orphan")
    ? await gateway.listPricingOverrides()
    : [];
  const states = await db
    .select({ id: modelPricePush.gatewayOverrideId })
    .from(modelPricePush)
    .where(sql`${modelPricePush.gatewayOverrideId} is not null`);
  const carried = new Set(states.map((state) => state.id));
  for (const override of overrides) {
    if (versionIdOf(override.name) !== undefined && !carried.has(override.id)) {
      await gateway.deletePricingOverride(override.id);
      orphansDeleted += 1;
    }
  }
  return { pushed, check, repaired, orphansDeleted };
};

/** For the admin list: whether each target's version in force is on the gateway. */
export const getPushStates = async () => db.select().from(modelPricePush);
