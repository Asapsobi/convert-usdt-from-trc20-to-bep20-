// The one place this app calls relayd's own /v1/relay-legs routes
// (relayd/internal/httpapi/relay_legs_handlers.go). Mirrors
// storefront/src/api/client.ts's own shape, with one real, confirmed
// difference: relayd's own error body is a FLAT {"error":"<string>"}
// (relayd/internal/httpapi/server.go's own writeError), not gateway's
// nested {error:{code,message}} -- ApiError below is constructed
// accordingly, with no separate `code` field since relayd's own errors
// don't carry one.
import type { CreateRelayLegResponse, Direction, QuoteResponse, RelayLeg } from "./types";

// No default -- an unconfigured deployment must fail loudly in dev
// rather than silently pointing at the wrong backend, the same
// discipline storefront's own client.ts already uses for gateway.
const RELAYD_BASE_URL = import.meta.env.VITE_RELAYD_BASE_URL as string | undefined;

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function request<T>(path: string, options: { method?: string; body?: unknown } = {}): Promise<T> {
  if (!RELAYD_BASE_URL) {
    throw new Error(
      "VITE_RELAYD_BASE_URL is not set -- see .env.example. This app refuses to guess which relayd to talk to.",
    );
  }
  const headers: Record<string, string> = {};
  if (options.body !== undefined) headers["Content-Type"] = "application/json";

  const resp = await fetch(`${RELAYD_BASE_URL}${path}`, {
    method: options.method ?? "GET",
    headers,
    body: options.body !== undefined ? JSON.stringify(options.body) : undefined,
  });

  const text = await resp.text();
  const data = text ? JSON.parse(text) : undefined;

  if (!resp.ok) {
    const errBody = data as { error?: string } | undefined;
    throw new ApiError(resp.status, errBody?.error ?? `request failed with status ${resp.status}`);
  }
  return data as T;
}

export const api = {
  quote: (direction: Direction, amountIn: string) =>
    request<QuoteResponse>("/v1/quotes", { method: "POST", body: { direction, amount_in: amountIn } }),

  createRelayLeg: (
    externalId: string,
    customerLabel: string,
    direction: Direction,
    destinationAddress: string,
    amountIn: string,
  ) =>
    request<CreateRelayLegResponse>("/v1/relay-legs", {
      method: "POST",
      body: {
        external_id: externalId,
        customer_label: customerLabel,
        direction,
        destination_address: destinationAddress,
        amount_in: amountIn,
      },
    }),

  getRelayLeg: (externalId: string) => request<RelayLeg>(`/v1/relay-legs/${encodeURIComponent(externalId)}`),
};
