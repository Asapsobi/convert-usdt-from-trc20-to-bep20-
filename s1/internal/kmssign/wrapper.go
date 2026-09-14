package kmssign

import (
	"context"
	"errors"
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// compactSigRecoveryBase and compactSigCompressedFlag mirror
// decred/dcrd/dcrec/secp256k1/v4/ecdsa's own unexported compact-signature
// constants (SignCompact/RecoverCompact's own doc comment: "The compact
// sig recovery code is the value 27 + public key recovery code + 4 if the
// compact signature was created with a compressed public key"). Wrapper
// never calls SignCompact itself (KMS produces a plain DER signature,
// never this format) -- it builds a compact-format buffer BY HAND purely
// to reuse RecoverCompact's own, already-correct, already-tested
// recovery math, for each of the two practical recovery codes.
const (
	compactSigRecoveryBase   = 27
	compactSigCompressedFlag = 4
)

// Wrapper turns a bare KMSClient into the real TRON-over-KMS signing
// mechanics docs/02-architecture/s1-key-custody-architecture.md's own
// "Signing mechanics" section describes. It is the only type in this
// module that ever calls KMSClient.Sign.
type Wrapper struct {
	client      KMSClient
	depositKeys *BSCDepositKeys
}

// NewWrapper returns a Wrapper calling client for every Sign. BSC
// deposit-address signing (SignBSCDeposit/PublicKeyForDeposit) starts
// disabled -- call SetBSCDepositKeys to enable it. Most deployments
// (and cmd/seed-slot-key, internal/replay's own fake harness) never
// need it.
func NewWrapper(client KMSClient) *Wrapper {
	return &Wrapper{client: client}
}

// ErrBSCDepositKeysNotConfigured is SignBSCDeposit/PublicKeyForDeposit's
// own result on a Wrapper that never had SetBSCDepositKeys called --
// this capability is opt-in, not assumed, since not every S1 deployment
// needs it (see bscdeposit.go's own top-of-file doc comment on why it's
// a genuine custody-model gap, not something to enable by default).
var ErrBSCDepositKeysNotConfigured = errors.New("kmssign: no BSC deposit keys configured on this Wrapper -- call SetBSCDepositKeys")

// SetBSCDepositKeys enables this Wrapper's own SignBSCDeposit and
// PublicKeyForDeposit methods, backed by keys.
func (w *Wrapper) SetBSCDepositKeys(keys *BSCDepositKeys) {
	w.depositKeys = keys
}

// PublicKeyForDeposit returns the compressed public key for the BSC
// deposit address at child index -- public information, safe to report
// freely (it's what the address itself is made from). Takes ctx for
// interface parity with GetPublicKey/Sign, even though this
// implementation is purely in-process -- a real derivation-capable
// KMS/HSM adapter behind this same signature would need it for the
// actual network call.
func (w *Wrapper) PublicKeyForDeposit(ctx context.Context, index uint32) ([33]byte, error) {
	if w.depositKeys == nil {
		return [33]byte{}, ErrBSCDepositKeysNotConfigured
	}
	return w.depositKeys.PublicKey(index)
}

// SignBSCDeposit is SignBSCDeposit's own real implementation: derive the
// child private key at index, sign digest with it in-process (see
// bscdeposit.go's own top-of-file doc comment on why this differs from
// Sign's KMS-mediated custody model), and apply the exact same low-s
// normalization and recovery-id matching against expectedPubKey that
// Sign applies to a real KMS response. Takes ctx for the same reason as
// PublicKeyForDeposit above.
func (w *Wrapper) SignBSCDeposit(ctx context.Context, index uint32, digest [32]byte, expectedPubKey [33]byte) ([65]byte, error) {
	if w.depositKeys == nil {
		return [65]byte{}, ErrBSCDepositKeysNotConfigured
	}
	der, err := w.depositKeys.sign(index, digest)
	if err != nil {
		return [65]byte{}, fmt.Errorf("kmssign: deriving/signing BSC deposit index %d: %w", index, err)
	}
	return finishRecoverableSignature(der, digest, expectedPubKey)
}

// GetPublicKey returns keyID's own compressed public key.
func (w *Wrapper) GetPublicKey(ctx context.Context, keyID string) ([33]byte, error) {
	der, err := w.client.GetPublicKey(ctx, keyID)
	if err != nil {
		return [33]byte{}, fmt.Errorf("kmssign: KMS GetPublicKey for key %s: %w", keyID, err)
	}
	return parseDERPublicKey(der)
}

// Sign produces a 65-byte r||s||v TRON/Ethereum-shaped signature over
// digest for keyID, verified against expectedPubKey before ever being
// returned. See this package's own doc comment and the architecture
// doc's "Signing mechanics" section for why each step below is
// necessary, not incidental:
//
//  1. Ask KMS to sign digest -- returns DER-encoded (r, s), no recovery
//     id.
//  2. Normalize s to the curve's canonical low-half form (TRON, like
//     Ethereum, rejects or is ambiguous about the non-canonical form;
//     KMS does not guarantee low-s on its own).
//  3. Recover the public key for both practical recovery codes (0 and
//     1 -- codes 2/3 correspond to the r-overflowed-the-field-prime
//     case, astronomically rare, and are not attempted, matching
//     Ethereum's own real-world convention of only ever producing v in
//     {27, 28}) and keep whichever one's recovered key matches
//     expectedPubKey.
//  4. Neither matches: ErrSignatureDoesNotMatchKey -- KMS returned a
//     signature that doesn't correspond to what was asked, a real,
//     serious condition, not a parsing failure.
func (w *Wrapper) Sign(ctx context.Context, keyID string, digest [32]byte, expectedPubKey [33]byte) ([65]byte, error) {
	der, err := w.client.Sign(ctx, keyID, digest)
	if err != nil {
		return [65]byte{}, fmt.Errorf("kmssign: KMS Sign for key %s: %w", keyID, err)
	}
	return finishRecoverableSignature(der, digest, expectedPubKey)
}

// finishRecoverableSignature is Sign's own steps 2-4, factored out so
// SignBSCDeposit (below) applies the identical low-s normalization and
// recovery-id matching to a DER signature produced by BSCDepositKeys's
// own in-process sign, rather than a real KMS response -- a caller of
// either method gets an identically-shaped guarantee regardless of which
// key type backed it.
func finishRecoverableSignature(der []byte, digest [32]byte, expectedPubKey [33]byte) ([65]byte, error) {
	sig, err := ecdsa.ParseDERSignature(der)
	if err != nil {
		return [65]byte{}, fmt.Errorf("%w: %v", ErrMalformedSignature, err)
	}

	r := sig.R()
	s := sig.S()
	if s.IsOverHalfOrder() {
		s.Negate()
	}
	var rBytes, sBytes [32]byte
	r.PutBytesUnchecked(rBytes[:])
	s.PutBytesUnchecked(sBytes[:])

	expected, err := secp256k1.ParsePubKey(expectedPubKey[:])
	if err != nil {
		return [65]byte{}, fmt.Errorf("%w: parsing expected public key: %v", ErrMalformedPublicKey, err)
	}

	for recoveryCode := byte(0); recoveryCode <= 1; recoveryCode++ {
		compact := make([]byte, 65)
		compact[0] = compactSigRecoveryBase + recoveryCode + compactSigCompressedFlag
		copy(compact[1:33], rBytes[:])
		copy(compact[33:65], sBytes[:])

		recovered, _, err := ecdsa.RecoverCompact(compact, digest[:])
		if err != nil {
			// This recovery code doesn't correspond to a valid curve
			// point for (r, s, digest) at all -- try the other one
			// before giving up.
			continue
		}
		if recovered.IsEqual(expected) {
			var out [65]byte
			copy(out[0:32], rBytes[:])
			copy(out[32:64], sBytes[:])
			out[64] = recoveryCode
			return out, nil
		}
	}
	return [65]byte{}, ErrSignatureDoesNotMatchKey
}
