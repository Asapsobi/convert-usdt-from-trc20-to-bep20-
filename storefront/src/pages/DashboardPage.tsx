import { useEffect, useState, type FormEvent } from "react";
import { useNavigate, Link } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError } from "../api/client";
import type { Quote, Tier } from "../api/types";
import { loadOrderIds, recordOrder } from "../api/localOrders";
import { isValidTronAddress, looksLikeEvmAddress } from "../api/addresses";

const TIER_OPTIONS: { value: Tier; label: string; hint: string }[] = [
  { value: "DIRECT", label: "Direct", hint: "Fastest — dispatched right away, higher fee floor." },
  { value: "STANDARD", label: "Standard", hint: "Usually settles within the hour." },
  { value: "SWEEP", label: "Economy", hint: "Batched with other orders — lowest fee, slower." },
];

function formatAmountInput(raw: string): string {
  const n = Number(raw);
  if (!Number.isFinite(n) || n <= 0) throw new Error("Enter a valid amount greater than 0.");
  return n.toFixed(6);
}

export function DashboardPage() {
  const { session } = useAuth();
  const navigate = useNavigate();

  const [tier, setTier] = useState<Tier>("STANDARD");
  const [amountIn, setAmountIn] = useState("");
  const [recipientAddress, setRecipientAddress] = useState("");
  const [quote, setQuote] = useState<Quote | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [quoting, setQuoting] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [recentOrders, setRecentOrders] = useState<string[]>([]);

  useEffect(() => {
    if (session) setRecentOrders(loadOrderIds(session.retailCustomerId));
  }, [session]);

  if (!session) return null;

  async function onGetQuote(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setQuote(null);

    const trimmed = recipientAddress.trim();
    if (!isValidTronAddress(trimmed)) {
      setError(
        looksLikeEvmAddress(trimmed)
          ? "That's a BSC-format address (starts with 0x). This corridor pays out on TRON — the destination must be a TRC20 address (starts with T)."
          : "Enter a valid TRC20 (TRON) destination address — it should start with 'T' and be 34 characters long.",
      );
      return;
    }

    setQuoting(true);
    try {
      const formatted = formatAmountInput(amountIn);
      const q = await api.createQuote(session!.token, tier, formatted, trimmed);
      setQuote(q);
    } catch (err) {
      if (err instanceof ApiError) setError(err.message);
      else setError(err instanceof Error ? err.message : "Could not get a quote.");
    } finally {
      setQuoting(false);
    }
  }

  async function onConfirm() {
    if (!quote) return;
    setError(null);
    setConfirming(true);
    try {
      const externalId = `web-${crypto.randomUUID()}`;
      await api.createOrder(session!.token, quote.quote_id, externalId);
      recordOrder(session!.retailCustomerId, externalId);
      navigate(`/orders/${externalId}`);
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.code === "quote_expired") {
          setError("That quote expired. Please get a new one.");
          setQuote(null);
        } else {
          setError(err.message);
        }
      } else {
        setError("Could not create the order.");
      }
    } finally {
      setConfirming(false);
    }
  }

  return (
    <div className="container">
      <div className="card">
        <h1>Convert USDT</h1>
        <p className="lede">Quote first — the price you confirm is the price you get.</p>

        <div className="direction-banner">
          <div>
            <div className="hint">You send</div>
            <div className="net-badge net-bsc">BSC · BEP20</div>
          </div>
          <div className="direction-arrow">→</div>
          <div>
            <div className="hint">You receive</div>
            <div className="net-badge net-tron">TRON · TRC20</div>
          </div>
        </div>

        {error && <div className="error-banner">{error}</div>}

        {!quote ? (
          <form onSubmit={onGetQuote}>
            <label htmlFor="tier">Speed</label>
            <select id="tier" value={tier} onChange={(e) => setTier(e.target.value as Tier)}>
              {TIER_OPTIONS.map((t) => (
                <option key={t.value} value={t.value}>
                  {t.label}
                </option>
              ))}
            </select>
            <div className="hint">{TIER_OPTIONS.find((t) => t.value === tier)?.hint}</div>

            <label htmlFor="amount">Amount to send, on BSC (BEP20 USDT)</label>
            <input
              id="amount"
              type="number"
              min="0"
              step="0.000001"
              required
              value={amountIn}
              onChange={(e) => setAmountIn(e.target.value)}
              placeholder="e.g. 500"
            />

            <label htmlFor="recipient">Destination address, on TRON (TRC20)</label>
            <input
              id="recipient"
              type="text"
              required
              value={recipientAddress}
              onChange={(e) => setRecipientAddress(e.target.value)}
              placeholder="A TRC20 address starting with T — this is where the converted USDT arrives"
            />

            <button type="submit" disabled={quoting}>
              {quoting ? "Getting quote…" : "Get quote"}
            </button>
          </form>
        ) : (
          <div>
            <div className="summary-row">
              <span className="k">You send</span>
              <span className="mono">{quote.amount_in} USDT</span>
            </div>
            <div className="summary-row">
              <span className="k">You receive</span>
              <span className="mono">{quote.amount_out} USDT</span>
            </div>
            <div className="summary-row">
              <span className="k">Fee</span>
              <span className="mono">{quote.fee_units} USDT</span>
            </div>
            <div className="summary-row">
              <span className="k">Network fee</span>
              <span className="mono">{quote.network_fee_units} USDT</span>
            </div>
            <div className="summary-row">
              <span className="k">Destination (TRC20)</span>
              <span className="mono">{quote.recipient_address}</span>
            </div>
            <div className="hint">
              Price locked until {new Date(quote.expires_at).toLocaleTimeString()} — confirm before then.
            </div>
            <div style={{ display: "flex", gap: 12 }}>
              <button onClick={onConfirm} disabled={confirming}>
                {confirming ? "Creating order…" : "Confirm"}
              </button>
              <button className="secondary" onClick={() => setQuote(null)} disabled={confirming}>
                Cancel
              </button>
            </div>
          </div>
        )}
      </div>

      {recentOrders.length > 0 && (
        <div className="card">
          <h2>Recent orders on this device</h2>
          <div className="hint" style={{ marginBottom: 8 }}>
            Only orders created from this browser are listed here.
          </div>
          {recentOrders.map((id) => (
            <Link key={id} className="order-list-item" to={`/orders/${id}`}>
              <span className="id mono">{id}</span>
              <span>View →</span>
            </Link>
          ))}
        </div>
      )}
    </div>
  );
}
