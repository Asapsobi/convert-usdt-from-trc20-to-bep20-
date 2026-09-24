// Mirrors gateway/docs/openapi.yaml's own retail schemas exactly --
// this file has no logic, only the shapes the backend actually returns,
// so a backend contract change is a compile error here, not a runtime
// surprise.

export type Tier = "DIRECT" | "STANDARD" | "SWEEP";

export interface RetailSession {
  session_token: string;
  retail_customer_id: number;
  expires_at: string;
}

export interface RetailMe {
  retail_customer_id: number;
  email: string;
}

export interface Quote {
  quote_id: number;
  tier: Tier;
  amount_in: string;
  amount_out: string;
  fee_units: string;
  network_fee_units: string;
  recipient_address: string;
  created_at: string;
  expires_at: string;
}

// The full status enum GET /v1/retail/orders/{id} can report, per
// status_handlers.go's own customerStates map plus the two
// gateway-local pre-C1 states (address_pending/created).
export type OrderStatus =
  | "address_pending"
  | "created"
  | "quoted"
  | "funded"
  | "screened"
  | "dispatching"
  | "settled"
  | "held"
  | "refunded"
  | "expired";

export interface Order {
  external_id: string;
  tier: Tier;
  amount_in: string;
  amount_out: string;
  fee_units: string;
  network_fee_units: string;
  recipient_address: string;
  deposit_address: string | null;
  status: OrderStatus;
}

export interface ApiErrorBody {
  error: { code: string; message: string };
}
