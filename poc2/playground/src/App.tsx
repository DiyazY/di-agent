import "./App.css";
import SystemPanel from "./components/SystemPanel";

export default function App() {
  return (
    <div className="app">
      <h1>di-agent Playground</h1>
      <p className="subtitle">
        Live control panel for the genset and propulsion simulators.
      </p>
      <div className="panels">
        <SystemPanel system="genset" />
        <SystemPanel system="propulsion" />
      </div>
    </div>
  );
}
