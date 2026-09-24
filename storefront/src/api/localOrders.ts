// C6's own /v1/retail/... surface has GET /v1/retail/orders/{id}
// (single order) but no list endpoint -- a real gap, not an oversight
// missed here: adding server-side order listing/pagination is its own
// scoped backend change, out of scope for "build the frontend." This
// is a deliberate, narrow workaround: track this browser's own
// recently-created order ids locally, per retail customer id, so the
// dashboard has *something* to show without inventing a fake list
// endpoint. It is NOT a durable order history -- a different browser
// or a cleared localStorage sees none of it. Say so in the UI, not
// just here.
const KEY_PREFIX = "storefront_orders_";

export function recordOrder(retailCustomerId: number, externalId: string) {
  const key = KEY_PREFIX + retailCustomerId;
  const existing = loadOrderIds(retailCustomerId);
  if (existing.includes(externalId)) return;
  localStorage.setItem(key, JSON.stringify([externalId, ...existing].slice(0, 20)));
}

export function loadOrderIds(retailCustomerId: number): string[] {
  const raw = localStorage.getItem(KEY_PREFIX + retailCustomerId);
  if (!raw) return [];
  try {
    const parsed = JSON.parse(raw);
    return Array.isArray(parsed) ? (parsed as string[]) : [];
  } catch {
    return [];
  }
}
