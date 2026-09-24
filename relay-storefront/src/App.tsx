import { BrowserRouter, Routes, Route, Link } from "react-router-dom";
import { CreateRelayLegPage } from "./pages/CreateRelayLegPage";
import { RelayLegStatusPage } from "./pages/RelayLegStatusPage";

export default function App() {
  return (
    <BrowserRouter>
      <div className="header">
        <Link to="/" className="brand">
          Model F Relay
        </Link>
      </div>
      <Routes>
        <Route path="/" element={<CreateRelayLegPage />} />
        <Route path="/legs/:externalId" element={<RelayLegStatusPage />} />
      </Routes>
    </BrowserRouter>
  );
}
