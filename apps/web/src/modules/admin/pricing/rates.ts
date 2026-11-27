import * as z from "zod";

/** Rates are stored in basis points (100 bps = 1%) and edited as percentages. */
export const bpsToPercent = (bps: number) => bps / 100;

export const percentToBps = (percent: number) => Math.round(percent * 100);

export { MICRO_SAR, sarToMicroSar } from "@workspace/shared/utils";

export const microSarToSar = (microSar: number) => microSar / 1_000_000;

/** A rate × 1,000,000 (3,750,000) and the decimal staff type (3.75). */
export const rateE6ToRate = (rateE6: number) => rateE6 / 1_000_000;

export const rateToRateE6 = (rate: number) => Math.round(rate * 1_000_000);

/** A percentage with at most two decimals, so it maps to whole basis points. */
export const percentSchema = (max: number, decimalsMessage: string) =>
  z
    .number()
    .min(0)
    .max(max)
    .refine((value) => Math.abs(value * 100 - Math.round(value * 100)) < 1e-9, {
      message: decimalsMessage,
    });

/** Riyals and halalas above zero, at most `max`: a top-up amount typed by staff. */
export const sarSchema = (max: number, decimalsMessage: string) =>
  z
    .number()
    .positive()
    .max(max)
    .refine((value) => Math.abs(value * 100 - Math.round(value * 100)) < 1e-9, {
      message: decimalsMessage,
    });

/** Provider names as the gateway uses them (apps/gateway/core/schemas). */
export const GATEWAY_PROVIDERS = [
  "openai",
  "azure",
  "anthropic",
  "bedrock",
  "bedrock_mantle",
  "sagemaker",
  "cohere",
  "vertex",
  "mistral",
  "ollama",
  "groq",
  "sgl",
  "parasail",
  "perplexity",
  "cerebras",
  "deepseek",
  "gemini",
  "openrouter",
  "elevenlabs",
  "huggingface",
  "nebius",
  "xai",
  "replicate",
  "vllm",
  "runway",
  "runware",
  "fireworks",
  "sarvam",
  "wafer",
  "databricks",
  "cloudflare",
  "typesafe",
  "minimax",
  "byteplus",
  "machgen",
  "baseten",
  "kimchi",
  "soniox",
] as const;
