import { useCallback, useEffect, useState } from "react";
import { fetchHealth, fetchStatus, setLoad, SystemName, SystemStatus } from "../api";

const POLL_INTERVAL_MS = 2000;

interface Props {
  system: SystemName;
}

export default function SystemPanel({ system }: Props) {
  const [status, setStatus] = useState<SystemStatus | null>(null);
  const [healthy, setHealthy] = useState<boolean | null>(null);
  const [sliderValue, setSliderValue] = useState(0);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      const [health, statusData] = await Promise.all([
        fetchHealth(system),
        fetchStatus(system),
      ]);
      setHealthy(health.status === "ok");
      setStatus(statusData);
      setError(null);
    } catch (err) {
      setHealthy(false);
      setError(err instanceof Error ? err.message : String(err));
    }
  }, [system]);

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

  const handleApply = async () => {
    setSubmitting(true);
    try {
      await setLoad(system, sliderValue / 100);
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
        {system}
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

      <div className="load-control">
        <label htmlFor={`${system}-slider`}>Set target load ratio</label>
        <div className="slider-row">
          <input
            id={`${system}-slider`}
            type="range"
            min={0}
            max={100}
            step={1}
            value={sliderValue}
            onChange={(e) => setSliderValue(Number(e.target.value))}
          />
          <span className="value">{sliderValue}%</span>
        </div>
        <button onClick={handleApply} disabled={submitting}>
          {submitting ? "Applying..." : "Apply"}
        </button>
      </div>

      {error && <p className="error-text">{error}</p>}
    </div>
  );
}
