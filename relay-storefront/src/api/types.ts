// Mirrors relayd/internal/httpapi/relay_legs_handlers.go's own real
// request/response shapes exactly -- this file has no logic, only the
// shapes the backend actually returns, so a backend contract change is
// a compile error here, not a runtime surprise (same convention
// storefront/src/api/types.ts already uses for gateway's own contract).

export type Direction = "TRC20_TO_BEP20" | "BEP20_TO_TRC20";

// The order_state values C1 (the ledger) can report for a RELAY-tier
// order, plus relay_status's own finer-grained values -- relayd has no
// single unified status enum of its own; a caller reads both fields (see
// getRelayLegResponse in relay_legs_handlers.go).
export type RelayStatus =
  | "AWAITING_DEPOSIT"
  | "SCREENED"
  | "HELD"
  | "FORWARDING"
  | "FORWARDED"
  | "SETTLED"
  | "REFUND_PENDING"
  | "REFUNDED"
  | "UNRECOVERABLE"
  | "EXPIRED";

// The money a customer sees before and after ordering: amount_in =
// our_fee + vendor_fee + amount_out (USDT, 6 decimals, both networks).
export interface Breakdown {
  amount_in: string;
  our_fee: string;
  vendor_fee: string;
  amount_out: string;
  vendor: string;
}

export interface QuoteResponse extends Breakdown {
  direction: Direction;
  valid_until: string;
}

export interface CreateRelayLegResponse extends Breakdown {
  external_id: string;
  order_id: number;
  deposit_address: string;
  deposit_deadline: string;
  amount_out_quoted: string;
  fee_units: string;
  quote_expires_at: string;
}

export interface RelayLeg {
  external_id: string;
  order_id: number;
  order_state: string;
  relay_status: RelayStatus;
  direction: Direction;
  deposit_address: string;
  destination_address: string;
  amount_in: string;
  amount_out_expected: string;
  amount_out_actual?: string;
  received_amount?: string;
  our_fee?: string;
  forward_amount?: string;
  vendor_fee?: string;
  vendor?: string;
  deposit_deadline: string;
  forward_tx_id?: string;
  refund_tx_id?: string;
}
