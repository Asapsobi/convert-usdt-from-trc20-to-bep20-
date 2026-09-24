import { Link } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";

export function LandingPage() {
  const { session } = useAuth();

  return (
    <div className="container">
      <div className="card">
        <h1>Convert USDT: BSC (BEP20) → TRON (TRC20)</h1>
        <p className="lede">
          Send USDT on BSC, receive it on TRON. Get a quote, send your deposit, and the converted USDT lands in
          your TRC20 wallet — no manual bridging.
        </p>
        {session ? (
          <Link to="/dashboard">
            <button>Go to dashboard</button>
          </Link>
        ) : (
          <div style={{ display: "flex", gap: 12 }}>
            <Link to="/register">
              <button>Get started</button>
            </Link>
            <Link to="/login">
              <button className="secondary">Log in</button>
            </Link>
          </div>
        )}
      </div>
    </div>
  );
}
