import { useCallback, useEffect, useRef, useState } from "react";
import { fetchStatus } from "../api";

interface Props {
  propulsionIds: string[];
}

const PORT_A = "Umeå";
const PORT_B = "Vaasa";

// Kvarken crossing: the sailed distance varies with routing (90-148 km /
// 48.4-80 nm), so each leg picks a distance in that range rather than a
// single fixed value.
const MIN_DISTANCE_KM = 90;
const MAX_DISTANCE_KM = 148;
const MIN_DISTANCE_NM = 48.4;
const MAX_DISTANCE_NM = 80;
const KM_PER_NM = 1.852;

const POLL_INTERVAL_MS = 2000;
// Speeds up the crossing for the demo: 1 real second of propulsion at speed
// advances the ship as if it had sailed this many simulated seconds.
const TIME_SCALE = 120;

type Direction = "outbound" | "return";

function randomLeg(): { km: number; nm: number } {
  const t = Math.random();
  return {
    km: MIN_DISTANCE_KM + t * (MAX_DISTANCE_KM - MIN_DISTANCE_KM),
    nm: MIN_DISTANCE_NM + t * (MAX_DISTANCE_NM - MIN_DISTANCE_NM),
  };
}

export default function VoyagePanel({ propulsionIds }: Props) {
  const [direction, setDirection] = useState<Direction>("outbound");
  const [leg, setLeg] = useState(randomLeg);
  const [distanceCoveredKm, setDistanceCoveredKm] = useState(0);
  const [speedKnots, setSpeedKnots] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const legRef = useRef(leg);
  legRef.current = leg;
  const speedRef = useRef(speedKnots);
  speedRef.current = speedKnots;

  const refresh = useCallback(async () => {
    if (propulsionIds.length === 0) {
      setSpeedKnots(0);
      return;
    }
    try {
      const statuses = await Promise.all(
        propulsionIds.map((id) => fetchStatus("propulsion", id))
      );
      // All propulsion drives move the same hull, so the ship's speed is the
      // fastest currently-reporting drive (a stale/idle unit reads 0, not NaN).
      const speed = statuses.reduce((max, status) => {
        const sample = status.last_message?.ship_speed_knots;
        return typeof sample === "number" ? Math.max(max, sample) : max;
      }, 0);
      setSpeedKnots(speed);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }, [propulsionIds]);

  useEffect(() => {
    refresh();
    const id = setInterval(refresh, POLL_INTERVAL_MS);
    return () => clearInterval(id);
  }, [refresh]);

  // Advances the ship along the current leg only while the propulsion system
  // (engine) is actually driving the ship, at a speed derived from it.
  useEffect(() => {
    const id = setInterval(() => {
      const currentSpeed = speedRef.current;
      if (currentSpeed <= 0) return;
      const distanceStepKm =
        currentSpeed * KM_PER_NM * ((POLL_INTERVAL_MS / 1000) * TIME_SCALE / 3600);
      setDistanceCoveredKm((covered) => {
        const next = covered + distanceStepKm;
        if (next >= legRef.current.km) {
          setDirection((d) => (d === "outbound" ? "return" : "outbound"));
          setLeg(randomLeg());
          return 0;
        }
        return next;
      });
    }, POLL_INTERVAL_MS);
    return () => clearInterval(id);
  }, []);

  const from = direction === "outbound" ? PORT_A : PORT_B;
  const to = direction === "outbound" ? PORT_B : PORT_A;
  const progressRatio = leg.km > 0 ? Math.min(1, distanceCoveredKm / leg.km) : 0;
  const remainingKm = Math.max(0, leg.km - distanceCoveredKm);
  const remainingNm = remainingKm / KM_PER_NM;
  const speedKmh = speedKnots * KM_PER_NM;
  const etaHr = speedKmh > 0.01 ? remainingKm / speedKmh : undefined;

  return (
    <div className="panel voyage-panel">
      <h2>voyage route</h2>
      <div className="voyage-ports">
        <span>{from}</span>
        <span className="voyage-arrow">→</span>
        <span>{to}</span>
      </div>
      <div className="voyage-progress-track">
        <div
          className="voyage-progress-fill"
          style={{ width: `${(progressRatio * 100).toFixed(1)}%` }}
        />
      </div>
      <div className="metric-row">
        <span>Leg distance</span>
        <strong>
          {leg.km.toFixed(0)} km / {leg.nm.toFixed(1)} nm
        </strong>
      </div>
      <div className="metric-row">
        <span>Remaining</span>
        <strong>
          {remainingKm.toFixed(0)} km / {remainingNm.toFixed(1)} nm
        </strong>
      </div>
      <div className="metric-row">
        <span>Ship speed</span>
        <strong>{speedKnots > 0 ? `${speedKnots.toFixed(1)} kn` : "stopped"}</strong>
      </div>
      <div className="metric-row">
        <span>ETA</span>
        <strong>{etaHr !== undefined ? `${etaHr.toFixed(1)} h` : "--"}</strong>
      </div>
      {propulsionIds.length === 0 && (
        <p className="hint-text">No propulsion instances deployed, position can't be tracked.</p>
      )}
      {error && <p className="error-text">{error}</p>}
    </div>
  );
}
