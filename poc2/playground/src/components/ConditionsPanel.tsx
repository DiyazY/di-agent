import { useState } from "react";
import { setLoad } from "../api";
import {
  MAX_PEOPLE_ON_BOARD,
  OCEAN_STATES,
  OceanState,
  recommendAuxLoadRatio,
  recommendPropulsionLoadRatio,
} from "../recommendations";

interface ConditionsPanelProps {
  propulsionIds: string[];
  auxloadIds: string[];
}

export default function ConditionsPanel({ propulsionIds, auxloadIds }: ConditionsPanelProps) {
  const [oceanState, setOceanState] = useState<OceanState>("calm");
  const [peopleOnBoard, setPeopleOnBoard] = useState(0);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [applied, setApplied] = useState(false);

  const recommendedPropulsionLoadRatio = recommendPropulsionLoadRatio(oceanState);
  const recommendedAuxLoadRatio = recommendAuxLoadRatio(peopleOnBoard);

  const handleApply = async () => {
    setSubmitting(true);
    setApplied(false);
    setError(null);
    try {
      await Promise.all([
        ...propulsionIds.map((id) => setLoad("propulsion", id, recommendedPropulsionLoadRatio)),
        ...auxloadIds.map((id) => setLoad("auxload", id, recommendedAuxLoadRatio)),
      ]);
      setApplied(true);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSubmitting(false);
    }
  };

  const noTargets = propulsionIds.length === 0 && auxloadIds.length === 0;

  return (
    <div className="panel">
      <h2>voyage conditions</h2>

      <label htmlFor="ocean-state-select">Sea state</label>
      <select
        id="ocean-state-select"
        value={oceanState}
        onChange={(e) => {
          setOceanState(e.target.value as OceanState);
          setApplied(false);
        }}
      >
        {OCEAN_STATES.map((state) => (
          <option key={state} value={state}>
            {state}
          </option>
        ))}
      </select>

      <label htmlFor="people-on-board-input">People on board</label>
      <div className="slider-row">
        <input
          id="people-on-board-input"
          type="range"
          min={0}
          max={MAX_PEOPLE_ON_BOARD}
          step={1}
          value={peopleOnBoard}
          onChange={(e) => {
            setPeopleOnBoard(Number(e.target.value));
            setApplied(false);
          }}
        />
        <span className="value">{peopleOnBoard}</span>
      </div>

      <div className="metric-row">
        <span>Recommended propulsion load</span>
        <strong>{(recommendedPropulsionLoadRatio * 100).toFixed(0)}%</strong>
      </div>
      <div className="metric-row">
        <span>Recommended auxiliary load</span>
        <strong>{(recommendedAuxLoadRatio * 100).toFixed(0)}%</strong>
      </div>

      <div className="load-control">
        <button onClick={handleApply} disabled={submitting || noTargets}>
          {submitting ? "Applying..." : "Apply recommendations"}
        </button>
        {noTargets && (
          <p className="hint-text">No propulsion or auxiliary-load instances deployed.</p>
        )}
        {applied && !error && <p className="hint-text">Recommendations applied.</p>}
      </div>

      {error && <p className="error-text">{error}</p>}
    </div>
  );
}
