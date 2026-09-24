// This file is Wrapper's own real KMSClient (provider.go) backed by
// Privy (privy.io) Server Wallets -- S1's SLOT keys' real custody, not
// to be confused with privytron.go's own PrivyTronDepositKeys (which is
// specifically the per-order TRON deposit-sweep registry). PrivyKMSClient
// is a peer of FakeKMSClient: a generic KMSClient with zero knowledge of
// "slot" vs "deposit" -- Wrapper, slots.Store, and everything above them
// need no changes at all to use it.
//
// Slot-key CREATION is deliberately NOT done by any code in this
// module -- it is a human-run, out-of-band operational step, the same
// posture docs/02-architecture/s1-key-custody-architecture.md's own "Key
// generation and bootstrapping" already documents for AWS KMS ("a
// procedure a human runs once per key... the moment of key creation is
// exactly where a scripting mistake is least recoverable"). An operator
// creates a chain_type=tron Privy wallet directly (e.g. via a single
// curl POST /wallets, or Privy's own dashboard), then registers its
// wallet id as a slot's kms_key_id via cmd/seed-slot-key, exactly as
// they would register a real AWS KMS key ARN. This type only ever reads
// an EXISTING wallet's public key and signs with it -- never mints one
// (see getTronWallet's own doc comment for why cmd/privy-probe, a
// diagnostic tool, is deliberately not reused for this either).
package kmssign

import (
	"context"
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// PrivyKMSClient implements KMSClient against real Privy Server
// Wallets. keyID is simply an already-existing Privy wallet's own id --
// the exact same shape FakeKMSClient's own keyID already has, and the
// exact same string slots.Store.Register stores as kms_key_id.
type PrivyKMSClient struct {
	client *privyClient
}

// NewPrivyKMSClient wires a PrivyKMSClient against Privy's real API,
// authenticated with appID/appSecret (PRIVY_APP_ID/PRIVY_APP_SECRET --
// the SAME Privy app already used for TRON deposit-key custody,
// privytron.go's own PrivyTronDepositKeys; independent wallets, one
// account).
func NewPrivyKMSClient(appID, appSecret string) *PrivyKMSClient {
	return &PrivyKMSClient{client: newPrivyClient(appID, appSecret)}
}

// GetPublicKey implements KMSClient -- fetches an EXISTING wallet's
// public key by id (privy_client.go's own getTronWallet, confirmed live
// to return the same shape wallet creation does) and DER-encodes it via
// marshalDERPublicKey (der.go's own existing helper, previously used
// only by FakeKMSClient) into the exact SubjectPublicKeyInfo shape
// Wrapper.GetPublicKey already expects. Never creates a wallet -- see
// this file's own top-of-file doc comment for why that's a deliberate,
// human-run, out-of-band step.
func (c *PrivyKMSClient) GetPublicKey(ctx context.Context, keyID string) ([]byte, error) {
	pubKey, err := c.client.getTronWallet(ctx, keyID)
	if err != nil {
		return nil, fmt.Errorf("kmssign: privy: fetching public key for slot key %s: %w", keyID, err)
	}
	der, err := marshalDERPublicKey(pubKey)
	if err != nil {
		return nil, fmt.Errorf("kmssign: privy: encoding public key for slot key %s: %w", keyID, err)
	}
	return der, nil
}

// Sign implements KMSClient -- signs digest via Privy's raw_sign
// (privy_client.go's own rawSignTron, already proven and already used
// by PrivyTronDepositKeys.Sign), then DER-encodes the resulting bare
// (r, s) via ecdsa.NewSignature(...).Serialize(). KMSClient's own
// contract requires DER, so Wrapper.Sign's existing, already-tested
// finishRecoverableSignature (DER-parsing, low-s normalization,
// recovery-id brute force) runs completely unchanged against this
// output -- this method is a thin adapter at the DER boundary, not a
// new signing path.
func (c *PrivyKMSClient) Sign(ctx context.Context, keyID string, digest [32]byte) ([]byte, error) {
	idemKey := fmt.Sprintf("s1-slot-sign-%s-%x", keyID, digest)
	rBytes, sBytes, err := c.client.rawSignTron(ctx, keyID, digest, idemKey)
	if err != nil {
		return nil, fmt.Errorf("kmssign: privy: signing with slot key %s: %w", keyID, err)
	}

	var r, s secp256k1.ModNScalar
	if overflow := r.SetBytes(&rBytes); overflow != 0 {
		return nil, fmt.Errorf("%w: r overflows the curve order", ErrMalformedSignature)
	}
	if overflow := s.SetBytes(&sBytes); overflow != 0 {
		return nil, fmt.Errorf("%w: s overflows the curve order", ErrMalformedSignature)
	}
	return ecdsa.NewSignature(&r, &s).Serialize(), nil
}
