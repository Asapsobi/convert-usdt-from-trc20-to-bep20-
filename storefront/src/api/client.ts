// The one place this app calls C6's own /v1/retail/... routes
// (gateway/internal/httpapi/retail_*.go). No auth persistence lives
// here -- that's AuthContext's own job; this file only knows how to
// make one authenticated (or anonymous) call and turn a non-2xx
// response into a real, typed ApiError.
import type { Order, Quote, RetailMe, RetailSession } from "./types";

// No default -- an unconfigured deployment must fail loudly in dev
// rather than silently pointing at the wrong backend, the same "no
// hardcoded defaults for anything real-money-shaped" discipline the
// Go backend uses throughout this repo.
const API_BASE_URL = import.meta.env.VITE_API_BASE_URL as string | undefined;

export class ApiError extends Error {
  code: string;
  status: number;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.code = code;
    this.status = status;
  }
}

async function request<T>(
  path: string,
  options: { method?: string; body?: unknown; token?: string } = {},
): Promise<T> {
  if (!API_BASE_URL) {
    throw new Error(
      "VITE_API_BASE_URL is not set -- see .env.example. This app refuses to guess which gateway to talk to.",
    );
  }
  const headers: Record<string, string> = {};
  if (options.body !== undefined) headers["Content-Type"] = "application/json";
  if (options.token) headers["Authorization"] = `Bearer ${options.token}`;

  const resp = await fetch(`${API_BASE_URL}${path}`, {
    method: options.method ?? "GET",
    headers,
    body: options.body !== undefined ? JSON.stringify(options.body) : undefined,
  });

  if (resp.status === 204) return undefined as T;

  const text = await resp.text();
  const data = text ? JSON.parse(text) : undefined;

  if (!resp.ok) {
    const errBody = data as { error?: { code?: string; message?: string } } | undefined;
    throw new ApiError(
      resp.status,
      errBody?.error?.code ?? "unknown_error",
      errBody?.error?.message ?? `request failed with status ${resp.status}`,
    );
  }
  return data as T;
}

export const api = {
  register: (email: string, password: string) =>
    request<RetailSession>("/v1/retail/register", { method: "POST", body: { email, password } }),

  login: (email: string, password: string) =>
    request<RetailSession>("/v1/retail/login", { method: "POST", body: { email, password } }),

  logout: (token: string) => request<void>("/v1/retail/logout", { method: "POST", token }),

  me: (token: string) => request<RetailMe>("/v1/retail/me", { token }),

  createQuote: (token: string, tier: string, amountIn: string, recipientAddress: string) =>
    request<Quote>("/v1/retail/quotes", {
      method: "POST",
      token,
      body: { tier, amount_in: amountIn, recipient_address: recipientAddress },
    }),

  createOrder: (token: string, quoteId: number, externalId: string) =>
    request<Order>("/v1/retail/orders", {
      method: "POST",
      token,
      body: { quote_id: quoteId, external_id: externalId },
    }),

  getOrder: (token: string, externalId: string) =>
    request<Order>(`/v1/retail/orders/${encodeURIComponent(externalId)}`, { token }),
};
