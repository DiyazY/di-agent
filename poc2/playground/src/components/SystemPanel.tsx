import { useCallback, useEffect, useState } from "react";
import {
  fetchHealth,
  fetchStatus,
  fetchSwitchboardStatus,
  setLoad,
  SystemName,
  SystemStatus,
} from "../api";

const POLL_INTERVAL_MS = 2000;

interface Props {
  system: SystemName;
  id: string;
}

export default function SystemPanel({ system, id }: Props) {
  const [status, setStatus] = useState<SystemStatus | null>(null);
  const [healthy, setHealthy] = useState<boolean | null>(null);
  const [sliderValue, setSliderValue] = useState(0);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Tracked for propulsion/auxload: the switchboard's total available
  // supply, used to gate the slider since consumers no longer see genset
  // power directly.
  const [switchboardSupplyKw, setSwitchboardSupplyKw] = useState<number | null>(null);

  const refresh = useCallback(async () => {
    try {
      const [health, statusData] = await Promise.all([
        fetchHealth(system, id),
        fetchStatus(system, id),
      ]);
      setHealthy(health.status === "ok");
      setStatus(statusData);
      if (system === "propulsion" || system === "auxload") {
        const switchboardStatus = await fetchSwitchboardStatus();
        setSwitchboardSupplyKw(switchboardStatus.available_supply_kw);
      }
      setError(null);
    } catch (err) {
      setHealthy(false);
      setError(err instanceof Error ? err.message : String(err));
    }
  }, [system, id]);

  useEffect(() => {
    refresh();
    const id = setInterval(refresh, POLL_INTERVAL_MS);
    return () => clearInterval(id);
  }, [refresh]);

  useEffect(() => {
    if (status) {
      setSliderValue(Math.round(status.target_load_ratio * 100));
    }
  }, [status?.target_load_ratio]);

  // Propulsion and the auxiliary load can only be loaded once the
  // switchboard has power to allocate.
  const switchboardPowerAvailable =
    (system !== "propulsion" && system !== "auxload") || (switchboardSupplyKw ?? 0) > 0;

  const handleApply = async () => {
    setSubmitting(true);
    try {
      await setLoad(system, id, sliderValue / 100);
      await refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="panel">
      <h2>
        <span
          className={`status-dot ${
            healthy === null ? "" : healthy ? "ok" : "error"
          }`}
        />
        {id}
      </h2>

      <div className="metric-row">
        <span>Current load</span>
        <strong>
          {status ? `${(status.current_load_ratio * 100).toFixed(1)}%` : "--"}
        </strong>
      </div>
      <div className="metric-row">
        <span>Target load</span>
        <strong>
          {status ? `${(status.target_load_ratio * 100).toFixed(1)}%` : "--"}
        </strong>
      </div>
      {(system === "propulsion" || system === "auxload") && (
        <div className="metric-row">
          <span>Allocated power</span>
          <strong>
            {status?.allocated_power_kw !== undefined
              ? `${status.allocated_power_kw.toFixed(1)} kW`
              : "--"}
          </strong>
        </div>
      )}
      {(system === "genset" || system === "propulsion") && (
        <div className="metric-row">
          <span>sSpeed</span>
          <strong>
            {status?.speed_rpm !== undefined
              ? `${status.speed_rpm.toFixed(1)} rpm`
              : "--"}
          </strong>
        </div>
      )}
      {system === "battery" && (
        <div className="metric-row">
          <span>State of charge</span>
          <strong>
            {status?.soc !== undefined ? `${(status.soc * 100).toFixed(1)}%` : "--"}
          </strong>
        </div>
      )}

      <div className="load-control">
        <label htmlFor={`${id}-slider`}>Set target load ratio</label>
        <div className="slider-row">
          <input
            id={`${id}-slider`}
            type="range"
            min={0}
            max={100}
            step={1}
            value={sliderValue}
            disabled={!switchboardPowerAvailable}
            onChange={(e) => setSliderValue(Number(e.target.value))}
          />
          <span className="value">{sliderValue}%</span>
        </div>
        <button
          onClick={handleApply}
          disabled={submitting || !switchboardPowerAvailable}
        >
          {submitting ? "Applying..." : "Apply"}
        </button>
        {!switchboardPowerAvailable && (
          <p className="hint-text">
            Switchboard has no power available, load can't be adjusted.
          </p>
        )}
      </div>

      {error && <p className="error-text">{error}</p>}
    </div>
  );
}
