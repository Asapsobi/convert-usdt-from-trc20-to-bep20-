package kmssign

import (
	"context"
	"crypto/sha256"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

func TestWrapper_SignProducesAnIndependentlyVerifiableSignature(t *testing.T) {
	ctx := context.Background()
	fake := NewFakeKMSClient(1)
	w := NewWrapper(fake)

	pubKey, err := w.GetPublicKey(ctx, "slot-1")
	if err != nil {
		t.Fatalf("GetPublicKey: %v", err)
	}

	// Try several digests: recovery code depends on the random nonce's
	// own y-parity, which this loop can't force -- collecting whichever
	// codes come up naturally across enough digests proves both 0 and 1
	// are handled, not just whichever one happened to come up once.
	seenV := map[byte]bool{}
	for i := 0; i < 40; i++ {
		digest := sha256.Sum256([]byte(fmt.Sprintf("digest-%d", i)))
		sig, err := w.Sign(ctx, "slot-1", digest, pubKey)
		if err != nil {
			t.Fatalf("Sign (digest %d): %v", i, err)
		}
		seenV[sig[64]] = true

		if !verifyRecoverableSignature(t, sig, digest, pubKey) {
			t.Fatalf("Sign (digest %d): produced signature does not independently recover to the expected public key", i)
		}
	}
	if !seenV[0] || !seenV[1] {
		t.Fatalf("across 40 signatures, only saw recovery codes %v -- want both 0 and 1 to appear", keysOf(seenV))
	}
}

func keysOf(m map[byte]bool) []byte {
	var out []byte
	for k := range m {
		out = append(out, k)
	}
	return out
}

// verifyRecoverableSignature independently re-derives the public key from
// (digest, sig) via ecdsa.RecoverCompact -- built from sig's own r, s,
// and recovery byte exactly the way a real TRON node would reconstruct
// the compact form to verify the signature -- and checks it matches
// expectedPubKey, deliberately not just trusting Wrapper's own "it
// matched" return value. The same "verify by an independent path"
// discipline this project applies elsewhere (e.g. energybroker's own
// vendor cost reconciliation).
func verifyRecoverableSignature(t *testing.T, sig [65]byte, digest [32]byte, expectedPubKey [33]byte) bool {
	t.Helper()
	expected, err := secp256k1.ParsePubKey(expectedPubKey[:])
	if err != nil {
		t.Fatalf("parsing expected pub key: %v", err)
	}

	var s secp256k1.ModNScalar
	if s.SetByteSlice(sig[32:64]) {
		t.Fatal("signature S overflows the curve order")
	}
	if s.IsOverHalfOrder() {
		t.Fatal("returned signature S is not canonical (over half order) -- Wrapper.Sign must always normalize")
	}

	compact := make([]byte, 65)
	compact[0] = compactSigRecoveryBase + sig[64] + compactSigCompressedFlag
	copy(compact[1:33], sig[0:32])
	copy(compact[33:65], sig[32:64])

	recovered, _, err := ecdsa.RecoverCompact(compact, digest[:])
	if err != nil {
		t.Logf("RecoverCompact failed: %v", err)
		return false
	}
	return recovered.IsEqual(expected)
}

func TestWrapper_RejectsASignatureOverTheWrongDigest(t *testing.T) {
	ctx := context.Background()
	fake := NewFakeKMSClient(1)
	fake.ForceWrongDigestSignature("slot-1")
	w := NewWrapper(fake)

	pubKey, err := w.GetPublicKey(ctx, "slot-1")
	if err != nil {
		t.Fatalf("GetPublicKey: %v", err)
	}

	digest := sha256.Sum256([]byte("some transaction"))
	_, err = w.Sign(ctx, "slot-1", digest, pubKey)
	if !errors.Is(err, ErrSignatureDoesNotMatchKey) {
		t.Fatalf("Sign() error = %v, want ErrSignatureDoesNotMatchKey", err)
	}
}

func TestWrapper_NormalizesANonCanonicalHighSSignature(t *testing.T) {
	ctx := context.Background()
	fake := NewFakeKMSClient(1)
	w := NewWrapper(fake)

	pubKey, err := w.GetPublicKey(ctx, "slot-1")
	if err != nil {
		t.Fatalf("GetPublicKey: %v", err)
	}
	digest := sha256.Sum256([]byte("normalize me"))

	// Get a real, valid, low-s signature from the fake first, then
	// rebuild a high-s (non-canonical) DER encoding of the SAME (r, s)
	// pair by negating s -- both s and curve_order - s are valid
	// signatures for the same (message, key) per ECDSA's own structure,
	// so this is still a genuine signature, just in the non-canonical
	// form a real cloud KMS is free to return (it makes no low-s
	// promise) and Wrapper.Sign must correct.
	lowSDER, err := fake.Sign(ctx, "slot-1", digest)
	if err != nil {
		t.Fatalf("fake.Sign: %v", err)
	}
	highSDER := negateSInDERSignature(t, lowSDER)

	override := &fixedResponseKMSClient{inner: fake, signResponse: highSDER}
	w2 := NewWrapper(override)

	sig, err := w2.Sign(ctx, "slot-1", digest, pubKey)
	if err != nil {
		t.Fatalf("Sign (from a hand-built high-s DER input): %v", err)
	}

	var s secp256k1.ModNScalar
	if s.SetByteSlice(sig[32:64]) {
		t.Fatal("returned S overflows the curve order")
	}
	if s.IsOverHalfOrder() {
		t.Fatal("Sign returned a non-canonical (high-s) signature; want it normalized to the low half")
	}
}

// TestFinishRecoverableSignatureRS_MatchesTheDERPath proves
// finishRecoverableSignatureRS -- the bridge Privy's own raw_sign (bare
// r,s, never DER) calls directly, bypassing finishRecoverableSignature's
// DER parsing entirely -- against a LOCALLY generated (r, s) pair, with
// zero Privy/network dependency: split a real DER signature from the
// fake KMS client into raw r/s bytes by hand (the same shape Privy's
// real API returns, per privy_client.go's own doc comment), then feed
// that straight into finishRecoverableSignatureRS and independently
// verify the result recovers to the expected public key.
func TestFinishRecoverableSignatureRS_MatchesTheDERPath(t *testing.T) {
	ctx := context.Background()
	fake := NewFakeKMSClient(1)
	w := NewWrapper(fake)

	pubKey, err := w.GetPublicKey(ctx, "slot-1")
	if err != nil {
		t.Fatalf("GetPublicKey: %v", err)
	}
	digest := sha256.Sum256([]byte("privy raw_sign shaped input"))

	der, err := fake.Sign(ctx, "slot-1", digest)
	if err != nil {
		t.Fatalf("fake.Sign: %v", err)
	}
	sig, err := ecdsa.ParseDERSignature(der)
	if err != nil {
		t.Fatalf("ParseDERSignature: %v", err)
	}
	r := sig.R()
	s := sig.S()
	var rBytes, sBytes [32]byte
	r.PutBytesUnchecked(rBytes[:])
	s.PutBytesUnchecked(sBytes[:])

	out, err := finishRecoverableSignatureRS(rBytes, sBytes, digest, pubKey)
	if err != nil {
		t.Fatalf("finishRecoverableSignatureRS: %v", err)
	}
	if !verifyRecoverableSignature(t, out, digest, pubKey) {
		t.Fatal("finishRecoverableSignatureRS produced a signature that does not independently recover to the expected public key")
	}
}

// TestFinishRecoverableSignatureRS_RejectsAMismatchedPublicKey is
// TestWrapper_RejectsASignatureOverTheWrongDigest's own raw-(r,s)-path
// counterpart -- a signature that is internally valid but doesn't
// correspond to expectedPubKey must never be returned as if it did.
func TestFinishRecoverableSignatureRS_RejectsAMismatchedPublicKey(t *testing.T) {
	ctx := context.Background()
	fakeA := NewFakeKMSClient(1)
	fakeB := NewFakeKMSClient(2) // different seed -> different keypair
	digest := sha256.Sum256([]byte("mismatched key test"))

	der, err := fakeA.Sign(ctx, "slot-1", digest)
	if err != nil {
		t.Fatalf("fakeA.Sign: %v", err)
	}
	sig, err := ecdsa.ParseDERSignature(der)
	if err != nil {
		t.Fatalf("ParseDERSignature: %v", err)
	}
	r := sig.R()
	s := sig.S()
	var rBytes, sBytes [32]byte
	r.PutBytesUnchecked(rBytes[:])
	s.PutBytesUnchecked(sBytes[:])

	wrongPubKey, err := NewWrapper(fakeB).GetPublicKey(ctx, "slot-1")
	if err != nil {
		t.Fatalf("GetPublicKey (fakeB): %v", err)
	}

	_, err = finishRecoverableSignatureRS(rBytes, sBytes, digest, wrongPubKey)
	if !errors.Is(err, ErrSignatureDoesNotMatchKey) {
		t.Fatalf("finishRecoverableSignatureRS() error = %v, want ErrSignatureDoesNotMatchKey", err)
	}
}

func TestWrapper_MalformedDERIsATypedWrappedError(t *testing.T) {
	ctx := context.Background()
	fake := NewFakeKMSClient(1)
	fake.ForceMalformed("slot-1")
	w := NewWrapper(fake)

	_, err := w.GetPublicKey(ctx, "slot-1")
	if !errors.Is(err, ErrForcedMalformed) {
		t.Fatalf("GetPublicKey() error = %v, want it to wrap ErrForcedMalformed", err)
	}

	pubKeyFake := NewFakeKMSClient(1) // separate, unforced instance for a valid pubkey
	wUnforced := NewWrapper(pubKeyFake)
	pubKey, err := wUnforced.GetPublicKey(ctx, "slot-1")
	if err != nil {
		t.Fatalf("GetPublicKey (unforced): %v", err)
	}

	override := &fixedResponseKMSClient{inner: fake, signResponse: []byte("not a DER signature")}
	w2 := NewWrapper(override)
	_, err = w2.Sign(ctx, "slot-1", [32]byte{1}, pubKey)
	if !errors.Is(err, ErrMalformedSignature) {
		t.Fatalf("Sign() error = %v, want ErrMalformedSignature", err)
	}
}

// negateSInDERSignature parses a valid, canonical (low-s) DER ECDSA
// signature and re-encodes it with s replaced by curve_order - s --
// still a mathematically valid signature over the same (digest, key),
// just in the non-canonical form Wrapper.Sign must normalize away.
func negateSInDERSignature(t *testing.T, der []byte) []byte {
	t.Helper()
	var parsed struct{ R, S *big.Int }
	if _, err := asn1.Unmarshal(der, &parsed); err != nil {
		t.Fatalf("parsing DER signature to negate: %v", err)
	}

	var s secp256k1.ModNScalar
	sBytes := make([]byte, 32)
	parsed.S.FillBytes(sBytes)
	if s.SetByteSlice(sBytes) {
		t.Fatal("S overflows the curve order")
	}
	s.Negate()
	var negatedSBytes [32]byte
	s.PutBytesUnchecked(negatedSBytes[:])

	out, err := asn1.Marshal(struct{ R, S *big.Int }{
		R: parsed.R,
		S: new(big.Int).SetBytes(negatedSBytes[:]),
	})
	if err != nil {
		t.Fatalf("re-marshaling negated-S DER signature: %v", err)
	}
	return out
}

// fixedResponseKMSClient wraps a real KMSClient for GetPublicKey but
// always returns a fixed byte slice from Sign -- a test-only seam for
// injecting a hand-built (and possibly invalid) signature response
// without needing FakeKMSClient itself to support every malformed shape
// a test might want.
type fixedResponseKMSClient struct {
	inner        KMSClient
	signResponse []byte
}

func (f *fixedResponseKMSClient) GetPublicKey(ctx context.Context, keyID string) ([]byte, error) {
	return f.inner.GetPublicKey(ctx, keyID)
}

func (f *fixedResponseKMSClient) Sign(ctx context.Context, keyID string, digest [32]byte) ([]byte, error) {
	return f.signResponse, nil
}
