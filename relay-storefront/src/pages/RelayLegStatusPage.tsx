import { useEffect, useRef, useState } from "react";
import { useParams, Link } from "react-router-dom";
import { api, ApiError } from "../api/client";
import type { RelayLeg, RelayStatus } from "../api/types";

const POLL_INTERVAL_MS = 5000;

// relay_status's own terminal values (relayd/internal/relay/relay.go) --
// once here, this leg will never move again, so polling stops.
const TERMINAL: RelayStatus[] = ["SETTLED", "REFUNDED", "UNRECOVERABLE", "EXPIRED"];

const STATUS_LABEL: Record<RelayStatus, string> = {
  AWAITING_DEPOSIT: "Awaiting your deposit",
  SCREENED: "Verifying deposit",
  HELD: "On hold — under review",
  FORWARDING: "Forwarding to vendor",
  FORWARDED: "Waiting on vendor",
  SETTLED: "Complete",
  REFUND_PENDING: "Refund in progress",
  REFUNDED: "Refunded",
  UNRECOVERABLE: "Needs manual review",
  EXPIRED: "Expired — no deposit arrived in time",
};

function statusClass(status: RelayStatus): string {
  if (status === "SETTLED") return "status-done";
  if (status === "REFUNDED" || status === "UNRECOVERABLE" || status === "EXPIRED") return "status-failed";
  if (status === "HELD") return "status-failed";
  if (status === "AWAITING_DEPOSIT") return "status-pending";
  return "status-progress";
}

const SEND_NETWORK: Record<string, string> = { BEP20_TO_TRC20: "BSC (BEP20)", TRC20_TO_BEP20: "TRON (TRC20)" };

export function RelayLegStatusPage() {
  const { externalId } = useParams<{ externalId: string }>();
  const [leg, setLeg] = useState<RelayLeg | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const timerRef = useRef<number | undefined>(undefined);

  useEffect(() => {
    if (!externalId) return;
    let cancelled = false;

    async function poll() {
      try {
        const l = await api.getRelayLeg(externalId!);
        if (cancelled) return;
        setLeg(l);
        setError(null);
        if (!TERMINAL.includes(l.relay_status)) {
          timerRef.current = window.setTimeout(poll, POLL_INTERVAL_MS);
        }
      } catch (err) {
        if (cancelled) return;
        setError(err instanceof ApiError ? err.message : "Could not load this order.");
      }
    }
    poll();

    return () => {
      cancelled = true;
      if (timerRef.current) window.clearTimeout(timerRef.current);
    };
  }, [externalId]);

  function copyAddress() {
    if (!leg?.deposit_address) return;
    navigator.clipboard.writeText(leg.deposit_address).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  }

  return (
    <div className="container">
      <div className="card">
        <Link to="/" className="hint">
          ← Back
        </Link>
        <h1 style={{ marginTop: 12 }}>Relay order status</h1>
        <div className="hint mono">{externalId}</div>

        {error && <div className="error-banner">{error}</div>}

        {!leg ? (
          <p className="lede">Loading…</p>
        ) : (
          <>
            <div style={{ margin: "16px 0" }}>
              <span className={`status-badge ${statusClass(leg.relay_status)}`}>
                {STATUS_LABEL[leg.relay_status]}
              </span>
            </div>

            {leg.relay_status === "EXPIRED" && (
              <div className="error-banner">
                This order expired before a deposit arrived. Do not send funds to its address — create a new order
                instead.
              </div>
            )}

            {leg.relay_status === "AWAITING_DEPOSIT" && (
            <div className="deposit-box">
              <div className="hint">
                Send {leg.amount_in} USDT on <strong>{SEND_NETWORK[leg.direction]}</strong> to this address before{" "}
                <strong>{new Date(leg.deposit_deadline).toLocaleString()}</strong>
              </div>
              <div className="addr mono">{leg.deposit_address}</div>
              <button className="secondary" onClick={copyAddress}>
                {copied ? "Copied!" : "Copy address"}
              </button>
              <div className="hint" style={{ marginTop: 10 }}>
                Only send on the network shown above. Funds sent on any other network cannot be recovered
                automatically.
              </div>
            </div>
            )}

            <div className="summary-row">
              <span className="k">{leg.received_amount ? "Quoted" : "You send"}</span>
              <span className="mono">{leg.amount_in} USDT</span>
            </div>
            {leg.received_amount && (
              <div className="summary-row">
                <span className="k">Received from you</span>
                <span className="mono">{leg.received_amount} USDT</span>
              </div>
            )}
            {leg.our_fee && (
              <div className="summary-row">
                <span className="k">Service fee</span>
                <span className="mono">− {leg.our_fee} USDT</span>
              </div>
            )}
            {leg.vendor_fee && (
              <div className="summary-row">
                <span className="k">Conversion and network fees</span>
                <span className="mono">− {leg.vendor_fee} USDT</span>
              </div>
            )}
            <div className="summary-row">
              <span className="k">You receive (expected)</span>
              <span className="mono">{leg.amount_out_expected} USDT</span>
            </div>
            {leg.amount_out_actual && (
              <div className="summary-row">
                <span className="k">You received</span>
                <span className="mono">{leg.amount_out_actual} USDT</span>
              </div>
            )}
            <div className="summary-row">
              <span className="k">Destination</span>
              <span className="mono">{leg.destination_address}</span>
            </div>
            {leg.payout_tx_id && (
              <div className="summary-row">
                <span className="k">Payout transaction (to your wallet)</span>
                <span className="mono">{leg.payout_tx_id}</span>
              </div>
            )}
            {leg.refund_tx_id && (
              <div className="summary-row">
                <span className="k">Refund transaction</span>
                <span className="mono">{leg.refund_tx_id}</span>
              </div>
            )}
            {leg.forward_tx_id && (
              <div className="summary-row">
                <span className="k">Forward tx (our own leg, not the vendor's payout)</span>
                <span className="mono">{leg.forward_tx_id}</span>
              </div>
            )}

            {!TERMINAL.includes(leg.relay_status) && (
              <p className="hint" style={{ marginTop: 16 }}>
                This page updates automatically — no need to refresh.
              </p>
            )}
          </>
        )}
      </div>
    </div>
  );
}
