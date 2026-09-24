// Model D's own corridor is fixed-direction: deposit BSC (BEP20),
// receive TRON (TRC20) -- never the reverse, never same-network. The
// gateway API itself does NOT validate recipient_address format at all
// (confirmed by reading gateway/internal/httpapi/quotes_handlers.go --
// it only checks non-empty), so this frontend is the only layer that
// catches a customer pasting the wrong-network address before a quote
// is even issued. This is a real, load-bearing check, not decoration:
// entering a BSC-format address here for what must be a TRC20 payout
// is exactly the mistake that surfaced this file's own existence.

// TRON's own base58check address shape: 'T' + 33 base58 characters (34
// total). This is a SHAPE check, not a full base58check checksum
// verification (that would need the same decode/checksum machinery
// depositwatcher/s1 already implement in Go) -- good enough to catch
// "this is obviously not a TRON address" (an 0x-prefixed EVM address,
// an empty string, garbage), not a guarantee the address is real or
// unfrozen. The real, final check happens on-chain when funds actually
// move, same as it always has for every other component in this repo.
const TRON_ADDRESS_RE = /^T[1-9A-HJ-NP-Za-km-z]{33}$/;

export function isValidTronAddress(address: string): boolean {
  return TRON_ADDRESS_RE.test(address.trim());
}

export function looksLikeEvmAddress(address: string): boolean {
  return /^0x[0-9a-fA-F]{40}$/.test(address.trim());
}
