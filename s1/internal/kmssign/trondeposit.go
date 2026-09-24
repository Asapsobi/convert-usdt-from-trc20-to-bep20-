// This file is bscdeposit.go's own TRON counterpart: per-order TRON
// deposit addresses, signed the same way relayd's forward_trc20.go now
// needs (mirroring forward_bep20.go's already-fixed BSC-deposit sweep).
// Deliberately thin -- BIP32 extended-key parsing and CKDpriv math are
// 100% chain-agnostic (same version bytes, same base58check alphabet,
// same HMAC-SHA512 arithmetic; TRON and BSC only diverge at address
// ENCODING, which lives in internal/slots/tron.go, never here), so this
// file reuses bscdeposit.go's own parseXprv/parseXpubForCrossCheck/
// ckdPrivChild/bscHardenedOffset directly rather than duplicating them.
//
// Same PRODUCTION CAVEAT as bscdeposit.go's own top-of-file doc comment:
// TronDepositKeys holds real derived private key material directly in
// this process's own memory, not behind a real KMS/HSM boundary. Never
// wire S1_TRON_DEPOSIT_XPRV to a real seed protecting real customer
// funds without resolving that first.
package kmssign

import (
	"context"
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// TronDepositSigner is the swappable per-order TRON deposit-signing
// backend Wrapper.SignTronDeposit/PublicKeyForTronDeposit delegate to.
// *TronDepositKeys (this file, local in-process BIP32) and
// *PrivyTronDepositKeys (privytron.go, real custody via Privy Server
// Wallets) both satisfy this identically -- it is the ENTIRE surface
// this package's own custody-model choice for TRON deposit signing
// touches; requests.Store, keyRef, and every HTTP route above this
// package never see which concrete type is behind it.
type TronDepositSigner interface {
	PublicKey(ctx context.Context, index uint32) ([33]byte, error)
	Sign(ctx context.Context, index uint32, digest [32]byte, expectedPubKey [33]byte) ([65]byte, error)
}

// TronDepositKeys is BSCDepositKeys's own TRON-deposit counterpart --
// same shape, same math, a different in-memory node (S1_TRON_DEPOSIT_XPRV/
// XPUB describe a different seed than S1_BSC_DEPOSIT_XPRV/XPUB, matching
// tronwatcher's own TRONWATCHER_XPUB being a distinct xpub from
// depositwatcher's WATCHER_XPUB).
type TronDepositKeys struct {
	chainCode  [32]byte
	privKey    *secp256k1.PrivateKey
	compressed [33]byte
}

// NewTronDepositKeys parses xprv and validates it against xpub -- see
// NewBSCDepositKeys's own doc comment for why both are required. xpub
// should be the exact same value configured as tronwatcher's own
// TRONWATCHER_XPUB.
func NewTronDepositKeys(xprv, xpub string) (*TronDepositKeys, error) {
	chainCode, priv, err := parseXprv(xprv)
	if err != nil {
		return nil, err
	}
	compressed := priv.PubKey().SerializeCompressed()
	var compressedArr [33]byte
	copy(compressedArr[:], compressed)

	xpubChainCode, xpubKey, err := parseXpubForCrossCheck(xpub)
	if err != nil {
		return nil, fmt.Errorf("kmssign: parsing S1_TRON_DEPOSIT_XPUB for cross-check: %w", err)
	}
	if chainCode != xpubChainCode || compressedArr != xpubKey {
		return nil, ErrTronXprvXpubMismatch
	}

	return &TronDepositKeys{chainCode: chainCode, privKey: priv, compressed: compressedArr}, nil
}

// deriveChild returns the non-hardened BIP32 child private key at index
// -- see ckdPrivChild's own doc comment (bscdeposit.go) for the math,
// shared as-is between both chains.
func (k *TronDepositKeys) deriveChild(index uint32) (*secp256k1.PrivateKey, error) {
	return ckdPrivChild(k.chainCode, k.privKey, k.compressed, index)
}

// PublicKey returns the compressed public key for child index -- see
// BSCDepositKeys.PublicKey's own doc comment. Takes ctx for
// TronDepositSigner interface parity with the Privy-backed
// implementation, even though this one is purely in-process and never
// touches it.
func (k *TronDepositKeys) PublicKey(ctx context.Context, index uint32) ([33]byte, error) {
	child, err := k.deriveChild(index)
	if err != nil {
		return [33]byte{}, err
	}
	var out [33]byte
	copy(out[:], child.PubKey().SerializeCompressed())
	return out, nil
}

// sign produces a DER-encoded ECDSA signature over digest using the
// child private key at index -- see BSCDepositKeys.sign's own doc
// comment.
func (k *TronDepositKeys) sign(index uint32, digest [32]byte) ([]byte, error) {
	child, err := k.deriveChild(index)
	if err != nil {
		return nil, err
	}
	sig := ecdsa.Sign(child, digest[:])
	return sig.Serialize(), nil
}

// Sign implements TronDepositSigner: derive the child private key at
// index, sign digest with it in-process, and apply the same low-s
// normalization and recovery-id matching finishRecoverableSignature
// already applies for every other signing path in this package.
func (k *TronDepositKeys) Sign(ctx context.Context, index uint32, digest [32]byte, expectedPubKey [33]byte) ([65]byte, error) {
	der, err := k.sign(index, digest)
	if err != nil {
		return [65]byte{}, fmt.Errorf("kmssign: deriving/signing TRON deposit index %d: %w", index, err)
	}
	return finishRecoverableSignature(der, digest, expectedPubKey)
}
