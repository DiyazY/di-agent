// Pure functions mapping voyage conditions to recommended load ratios for
// the propulsion and auxiliary-load systems. These are simple lookup/linear
// models (not physics-based), meant to give the operator a sensible
// starting point that they can still override via each system's own panel.

export type OceanState = "calm" | "moderate" | "storm";

export const OCEAN_STATES: OceanState[] = ["calm", "moderate", "storm"];

export const MAX_PEOPLE_ON_BOARD = 1000;

// Rougher seas need more propulsion power to hold speed/steerage way against
// wave and wind resistance, so the recommended load ratio increases with
// sea state.
const OCEAN_STATE_PROPULSION_LOAD_RATIO: Record<OceanState, number> = {
  calm: 0.3,
  moderate: 0.6,
  storm: 0.9,
};

// Hotel load (HVAC, lighting, galley, water systems, ...) scales with the
// number of people aboard: a floor for essential systems at 0 people, up to
// full load at the max capacity.
const MIN_AUXLOAD_RATIO = 0.2;
const MAX_AUXLOAD_RATIO = 1.0;

export function recommendPropulsionLoadRatio(oceanState: OceanState): number {
  return OCEAN_STATE_PROPULSION_LOAD_RATIO[oceanState];
}

export function recommendAuxLoadRatio(peopleOnBoard: number): number {
  const clamped = Math.max(0, Math.min(MAX_PEOPLE_ON_BOARD, peopleOnBoard));
  const fraction = clamped / MAX_PEOPLE_ON_BOARD;
  return MIN_AUXLOAD_RATIO + (MAX_AUXLOAD_RATIO - MIN_AUXLOAD_RATIO) * fraction;
}
