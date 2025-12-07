export type AllowedPropertyValues = string | number | boolean;

type TrackFunction = (
  event: string,
  data?: Record<string, AllowedPropertyValues>,
) => void;

type IdentifyFunction = (
  userId: string,
  traits?: Record<string, AllowedPropertyValues>,
) => void;

export interface AnalyticsProviderStrategy {
  track: TrackFunction;
  identify: IdentifyFunction;
  reset: () => void;
}
