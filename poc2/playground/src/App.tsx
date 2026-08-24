import "./App.css";
import SystemPanel from "./components/SystemPanel";
import SwitchboardPanel from "./components/SwitchboardPanel";

export default function App() {
  return (
    <div className="app">
      <h1>di-agent Playground</h1>
      <p className="subtitle">
        Live control panel for the genset and propulsion simulators.
      </p>
      <div className="panels">
        <SystemPanel system="genset" />
        <SwitchboardPanel />
        <SystemPanel system="propulsion" />
      </div>
    </div>
  );
}
