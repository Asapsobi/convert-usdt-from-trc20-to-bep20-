import { BrowserRouter, Routes, Route, Link } from "react-router-dom";
import { CreateRelayLegPage } from "./pages/CreateRelayLegPage";
import { RelayLegStatusPage } from "./pages/RelayLegStatusPage";

export default function App() {
  return (
    <BrowserRouter>
      <div className="header">
        <Link to="/" className="brand">
          USDT Network Converter
        </Link>
      </div>
      {import.meta.env.VITE_LOCAL_TEST_BANNER === "true" && (
        <div className="test-banner">
          Local test setup: these wallets use test keys stored on this computer. Never send real funds here.
        </div>
      )}
      <Routes>
        <Route path="/" element={<CreateRelayLegPage />} />
        <Route path="/legs/:externalId" element={<RelayLegStatusPage />} />
      </Routes>
    </BrowserRouter>
  );
}
