export const Flag = {
  DEMO: "DEMO",
} as const;

export type Flag = (typeof Flag)[keyof typeof Flag];
