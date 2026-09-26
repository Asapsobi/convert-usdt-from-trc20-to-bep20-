import { useState, type FormEvent } from "react";
import { useNavigate } from "react-router-dom";
import { api, ApiError } from "../api/client";
import type { Direction, QuoteResponse } from "../api/types";

const DIRECTION_OPTIONS: { value: Direction; sendLabel: string; sendBadge: string; receiveLabel: string; receiveBadge: string }[] = [
  { value: "BEP20_TO_TRC20", sendLabel: "BSC · BEP20", sendBadge: "net-bsc", receiveLabel: "TRON · TRC20", receiveBadge: "net-tron" },
  { value: "TRC20_TO_BEP20", sendLabel: "TRON · TRC20", sendBadge: "net-tron", receiveLabel: "BSC · BEP20", receiveBadge: "net-bsc" },
];

function formatAmountInput(raw: string): string {
  const n = Number(raw);
  if (!Number.isFinite(n) || n <= 0) throw new Error("Enter a valid amount greater than 0.");
  return n.toFixed(6);
}

function errorMessage(err: unknown, fallback: string): string {
  if (err instanceof ApiError) return err.message;
  return err instanceof Error ? err.message : fallback;
}

export function CreateRelayLegPage() {
  const navigate = useNavigate();

  const [direction, setDirection] = useState<Direction>("BEP20_TO_TRC20");
  const [customerLabel, setCustomerLabel] = useState("");
  const [destinationAddress, setDestinationAddress] = useState("");
  const [amountIn, setAmountIn] = useState("");
  const [quote, setQuote] = useState<QuoteResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const opt = DIRECTION_OPTIONS.find((o) => o.value === direction)!;

  // Any change to what is being converted invalidates the quote shown.
  function changed<T>(set: (v: T) => void) {
    return (v: T) => {
      set(v);
      setQuote(null);
    };
  }

  async function onGetQuote(e: FormEvent) {
    e.preventDefault();
    setError(null);
    if (!customerLabel.trim()) {
      setError("Enter your name or email so this order can be found again.");
      return;
    }
    if (!destinationAddress.trim()) {
      setError(`Enter the wallet on ${opt.receiveLabel} that should receive your funds.`);
      return;
    }
    setBusy(true);
    try {
      setQuote(await api.quote(direction, formatAmountInput(amountIn)));
    } catch (err) {
      setError(errorMessage(err, "Could not price this conversion."));
    } finally {
      setBusy(false);
    }
  }

  async function onConfirm() {
    if (!quote) return;
    setError(null);
    setBusy(true);
    try {
      const externalId = `relay-web-${crypto.randomUUID()}`;
      await api.createRelayLeg(externalId, customerLabel.trim(), direction, destinationAddress.trim(), quote.amount_in);
      navigate(`/legs/${externalId}`);
    } catch (err) {
      if (err instanceof ApiError && err.status === 503) {
        setError("All deposit wallets are busy right now. Please try again in a few minutes.");
      } else {
        setError(errorMessage(err, "Could not create this order."));
      }
      setQuote(null);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="container">
      <div className="card">
        <h1>Convert USDT between networks</h1>
        <p className="lede">
          Send USDT on one network and receive it on the other, in your own wallet. You see exactly what you will
          receive before you confirm.
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

        <form onSubmit={onGetQuote}>
          <label htmlFor="direction">Direction</label>
          <select id="direction" value={direction} onChange={(e) => changed(setDirection)(e.target.value as Direction)}>
            {DIRECTION_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>
                {o.sendLabel} → {o.receiveLabel}
              </option>
            ))}
          </select>

          <label htmlFor="customerLabel">Your name or email</label>
          <input
            id="customerLabel"
            type="text"
            required
            value={customerLabel}
            onChange={(e) => setCustomerLabel(e.target.value)}
            placeholder="No account needed — just so this order can be found again"
          />

          <label htmlFor="amount">Amount to send, on {opt.sendLabel}</label>
          <input
            id="amount"
            type="number"
            min="0"
            step="0.000001"
            required
            value={amountIn}
            onChange={(e) => changed(setAmountIn)(e.target.value)}
            placeholder="e.g. 100"
          />

          <label htmlFor="destination">Your wallet on {opt.receiveLabel}</label>
          <input
            id="destination"
            type="text"
            required
            value={destinationAddress}
            onChange={(e) => changed(setDestinationAddress)(e.target.value)}
            placeholder="Where your converted USDT is sent"
          />

          {!quote && (
            <button type="submit" disabled={busy}>
              {busy ? "Pricing…" : "See what I'll receive"}
            </button>
          )}
        </form>

        {quote && (
          <div style={{ marginTop: 16 }}>
            <div className="summary-row">
              <span className="k">You send</span>
              <span className="mono">{quote.amount_in} USDT</span>
            </div>
            <div className="summary-row">
              <span className="k">Service fee</span>
              <span className="mono">− {quote.our_fee} USDT</span>
            </div>
            <div className="summary-row">
              <span className="k">Conversion and network fees</span>
              <span className="mono">− {quote.vendor_fee} USDT</span>
            </div>
            <div className="summary-row">
              <span className="k">
                <strong>You receive</strong>
              </span>
              <span className="mono">
                <strong>≈ {quote.amount_out} USDT</strong>
              </span>
            </div>
            <p className="hint">
              The conversion rate is set when your deposit arrives; the amount received may differ slightly. If you
              send more or less than {quote.amount_in} USDT, the fees are applied to what actually arrives.
            </p>
            <button onClick={onConfirm} disabled={busy}>
              {busy ? "Creating…" : "Confirm and get deposit address"}
            </button>
            <button className="secondary" onClick={() => setQuote(null)} disabled={busy} style={{ marginLeft: 8 }}>
              Change
            </button>
          </div>
        )}
      </div>
    </div>
  );
}
