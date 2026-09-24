import { useCallback, useEffect, useState } from "react";
import {
  fetchShorePowerHealth,
  fetchShorePowerStatus,
  setShorePowerConnected,
  setShorePowerRatio,
  ShorePowerStatus,
} from "../api";

const POLL_INTERVAL_MS = 2000;

export default function ShorePowerPanel() {
  const [status, setStatus] = useState<ShorePowerStatus | null>(null);
  const [healthy, setHealthy] = useState<boolean | null>(null);
  const [sliderValue, setSliderValue] = useState(0);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      const [health, statusData] = await Promise.all([
        fetchShorePowerHealth(),
        fetchShorePowerStatus(),
      ]);
      setHealthy(health.status === "ok");
      setStatus(statusData);
      setError(null);
    } catch (err) {
      setHealthy(false);
      setError(err instanceof Error ? err.message : String(err));
    }
  }, []);

  useEffect(() => {
    refresh();
    const id = setInterval(refresh, POLL_INTERVAL_MS);
    return () => clearInterval(id);
  }, [refresh]);

  useEffect(() => {
    if (status) {
      setSliderValue(Math.round(status.target_power_ratio * 100));
    }
  }, [status?.target_power_ratio]);

  const handleToggleConnected = async () => {
    setSubmitting(true);
    try {
      await setShorePowerConnected(!status?.connected);
      await refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSubmitting(false);
    }
  };

  const handleApply = async () => {
    setSubmitting(true);
    try {
      await setShorePowerRatio(sliderValue / 100);
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
        shore-power
      </h2>

      <div className="metric-row">
        <span>Connected</span>
        <strong>{status ? (status.connected ? "yes" : "no") : "--"}</strong>
      </div>
      <div className="metric-row">
        <span>Current power ratio</span>
        <strong>
          {status ? `${(status.current_power_ratio * 100).toFixed(1)}%` : "--"}
        </strong>
      </div>
      <div className="metric-row">
        <span>Power delivered (charging battery)</span>
        <strong>
          {status?.last_message?.power_kw !== undefined
            ? `${status.last_message.power_kw.toFixed(1)} kW`
            : "--"}
        </strong>
      </div>
      <div className="metric-row">
        <span>Converter losses</span>
        <strong>
          {status?.last_message?.losses_kw !== undefined
            ? `${status.last_message.losses_kw.toFixed(2)} kW`
            : "--"}
        </strong>
      </div>

      <div className="load-control">
        <button onClick={handleToggleConnected} disabled={submitting}>
          {status?.connected ? "Disconnect" : "Connect"}
        </button>
        <label htmlFor="shore-power-slider">Set target power ratio</label>
        <div className="slider-row">
          <input
            id="shore-power-slider"
            type="range"
            min={0}
            max={100}
            step={1}
            value={sliderValue}
            disabled={!status?.connected}
            onChange={(e) => setSliderValue(Number(e.target.value))}
          />
          <span className="value">{sliderValue}%</span>
        </div>
        <button onClick={handleApply} disabled={submitting || !status?.connected}>
          {submitting ? "Applying..." : "Apply"}
        </button>
        {!status?.connected && (
          <p className="hint-text">Connect shore power before setting a power ratio.</p>
        )}
      </div>

      {error && <p className="error-text">{error}</p>}
    </div>
  );
}
