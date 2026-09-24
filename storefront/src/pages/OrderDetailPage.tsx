import { useEffect, useRef, useState } from "react";
import { useParams, Link } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError } from "../api/client";
import type { Order, OrderStatus } from "../api/types";

const POLL_INTERVAL_MS = 5000;

const TERMINAL: OrderStatus[] = ["settled", "refunded", "expired"];

const STATUS_LABEL: Record<OrderStatus, string> = {
  address_pending: "Preparing deposit address",
  created: "Awaiting your deposit",
  quoted: "Awaiting your deposit",
  funded: "Deposit received",
  screened: "Verifying deposit",
  dispatching: "Sending your funds",
  settled: "Complete",
  held: "On hold — under review",
  refunded: "Refunded",
  expired: "Expired",
};

function statusClass(status: OrderStatus): string {
  if (status === "settled") return "status-done";
  if (status === "refunded" || status === "expired") return "status-failed";
  if (status === "held") return "status-failed";
  if (status === "created" || status === "quoted" || status === "address_pending") return "status-pending";
  return "status-progress";
}

export function OrderDetailPage() {
  const { externalId } = useParams<{ externalId: string }>();
  const { session } = useAuth();
  const [order, setOrder] = useState<Order | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const timerRef = useRef<number | undefined>(undefined);

  useEffect(() => {
    if (!session || !externalId) return;
    let cancelled = false;

    async function poll() {
      try {
        const o = await api.getOrder(session!.token, externalId!);
        if (cancelled) return;
        setOrder(o);
        setError(null);
        if (!TERMINAL.includes(o.status)) {
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
  }, [session, externalId]);

  if (!session) return null;

  function copyAddress() {
    if (!order?.deposit_address) return;
    navigator.clipboard.writeText(order.deposit_address).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  }

  return (
    <div className="container">
      <div className="card">
        <Link to="/dashboard" className="hint">
          ← Back
        </Link>
        <h1 style={{ marginTop: 12 }}>Order status</h1>
        <div className="hint mono">{externalId}</div>

        {error && <div className="error-banner">{error}</div>}

        {!order ? (
          <p className="lede">Loading…</p>
        ) : (
          <>
            <div style={{ margin: "16px 0" }}>
              <span className={`status-badge ${statusClass(order.status)}`}>{STATUS_LABEL[order.status]}</span>
            </div>

            {order.deposit_address ? (
              <div className="deposit-box">
                <div className="hint">
                  Send exactly {order.amount_in} USDT on <strong>BSC (BEP20)</strong> to this address
                </div>
                <div className="addr mono">{order.deposit_address}</div>
                <button className="secondary" onClick={copyAddress}>
                  {copied ? "Copied!" : "Copy address"}
                </button>
                <div className="hint" style={{ marginTop: 10 }}>
                  Only send BEP20 USDT here. Funds sent on any other network cannot be recovered automatically.
                </div>
              </div>
            ) : (
              <p className="hint">Preparing your deposit address — this usually takes a few seconds.</p>
            )}

            <div className="summary-row">
              <span className="k">You send (BSC · BEP20)</span>
              <span className="mono">{order.amount_in} USDT</span>
            </div>
            <div className="summary-row">
              <span className="k">You receive (TRON · TRC20)</span>
              <span className="mono">{order.amount_out} USDT</span>
            </div>
            <div className="summary-row">
              <span className="k">Destination (TRC20)</span>
              <span className="mono">{order.recipient_address}</span>
            </div>

            {!TERMINAL.includes(order.status) && (
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
