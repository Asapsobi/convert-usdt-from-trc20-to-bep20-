import { useState, type FormEvent } from "react";
import { useNavigate } from "react-router-dom";
import { api, ApiError } from "../api/client";
import type { Direction } from "../api/types";

const DIRECTION_OPTIONS: { value: Direction; sendLabel: string; sendBadge: string; receiveLabel: string; receiveBadge: string }[] = [
  { value: "BEP20_TO_TRC20", sendLabel: "BSC · BEP20", sendBadge: "net-bsc", receiveLabel: "TRON · TRC20", receiveBadge: "net-tron" },
  { value: "TRC20_TO_BEP20", sendLabel: "TRON · TRC20", sendBadge: "net-tron", receiveLabel: "BSC · BEP20", receiveBadge: "net-bsc" },
];

function formatAmountInput(raw: string): string {
  const n = Number(raw);
  if (!Number.isFinite(n) || n <= 0) throw new Error("Enter a valid amount greater than 0.");
  return n.toFixed(6);
}

export function CreateRelayLegPage() {
  const navigate = useNavigate();

  const [direction, setDirection] = useState<Direction>("BEP20_TO_TRC20");
  const [customerId, setCustomerId] = useState("");
  const [destinationAddress, setDestinationAddress] = useState("");
  const [amountIn, setAmountIn] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const opt = DIRECTION_OPTIONS.find((o) => o.value === direction)!;

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setError(null);

    if (!customerId.trim()) {
      setError("Enter your name or email so this order can be found again.");
      return;
    }

    setSubmitting(true);
    try {
      const formatted = formatAmountInput(amountIn);
      const externalId = `relay-web-${crypto.randomUUID()}`;
      await api.createRelayLeg(externalId, customerId.trim(), direction, destinationAddress.trim(), formatted);
      navigate(`/legs/${externalId}`);
    } catch (err) {
      if (err instanceof ApiError) setError(err.message);
      else setError(err instanceof Error ? err.message : "Could not create this order.");
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div className="container">
      <div className="card">
        <h1>Model F relay</h1>
        <p className="lede">
          Zero-float relay: send one asset, an upstream vendor swaps it, your own wallet receives the other. One
          step — no separate quote-then-confirm.
        </p>

        <div className="direction-banner">
          <div>
            <div className="hint">You send</div>
            <div className={`net-badge ${opt.sendBadge}`}>{opt.sendLabel}</div>
          </div>
          <div className="direction-arrow">→</div>
          <div>
            <div className="hint">You receive</div>
            <div className={`net-badge ${opt.receiveBadge}`}>{opt.receiveLabel}</div>
          </div>
        </div>

        {error && <div className="error-banner">{error}</div>}

        <form onSubmit={onSubmit}>
          <label htmlFor="direction">Direction</label>
          <select id="direction" value={direction} onChange={(e) => setDirection(e.target.value as Direction)}>
            {DIRECTION_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>
                {o.sendLabel} → {o.receiveLabel}
              </option>
            ))}
          </select>

          <label htmlFor="customerId">Your name or email</label>
          <input
            id="customerId"
            type="text"
            required
            value={customerId}
            onChange={(e) => setCustomerId(e.target.value)}
            placeholder="No account needed — just an identifier for this order"
          />
          <div className="hint">
            relayd has no login yet — this is passed through as a plain identifier, not a real account.
          </div>

          <label htmlFor="amount">Amount to send, on {opt.sendLabel}</label>
          <input
            id="amount"
            type="number"
            min="0"
            step="0.000001"
            required
            value={amountIn}
            onChange={(e) => setAmountIn(e.target.value)}
            placeholder="e.g. 100"
          />

          <label htmlFor="destination">Your destination wallet, on {opt.receiveLabel}</label>
          <input
            id="destination"
            type="text"
            required
            value={destinationAddress}
            onChange={(e) => setDestinationAddress(e.target.value)}
            placeholder="Where the vendor sends your converted funds"
          />

          <button type="submit" disabled={submitting}>
            {submitting ? "Creating…" : "Create order"}
          </button>
        </form>
      </div>
    </div>
  );
}
