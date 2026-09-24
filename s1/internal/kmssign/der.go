package kmssign

import (
	"encoding/asn1"
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// idECPublicKey and idSecp256k1 are the two OIDs that must both appear in
// a DER SubjectPublicKeyInfo for this package to trust it names a
// secp256k1 EC public key -- the same two OIDs AWS KMS's own GetPublicKey
// response carries for an ECC_SECG_P256K1 key. Go's stdlib crypto/x509
// does not recognize secp256k1 (only the NIST P-curves), so this package
// parses the SPKI structure directly against encoding/asn1 rather than
// through x509.ParsePKIXPublicKey -- the same "write the small thing
// stdlib doesn't cover, directly against primitives" choice
// depositwatcher/internal/addresses/bip32.go already made for the same
// curve, for a different purpose.
var (
	idECPublicKey = asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}
	idSecp256k1   = asn1.ObjectIdentifier{1, 3, 132, 0, 10}
)

type subjectPublicKeyInfo struct {
	Algorithm struct {
		Algorithm  asn1.ObjectIdentifier
		Parameters asn1.ObjectIdentifier
	}
	PublicKey asn1.BitString
}

// parseDERPublicKey parses a DER SubjectPublicKeyInfo (KMS's own
// GetPublicKey response shape) into a compressed secp256k1 public key,
// rejecting anything not explicitly on this curve -- a KMS response
// naming any other algorithm or curve is a configuration error worth
// failing loudly on, not coercing.
func parseDERPublicKey(der []byte) ([33]byte, error) {
	var spki subjectPublicKeyInfo
	rest, err := asn1.Unmarshal(der, &spki)
	if err != nil {
		return [33]byte{}, fmt.Errorf("%w: %v", ErrMalformedPublicKey, err)
	}
	if len(rest) != 0 {
		return [33]byte{}, fmt.Errorf("%w: trailing data after SubjectPublicKeyInfo", ErrMalformedPublicKey)
	}
	if !spki.Algorithm.Algorithm.Equal(idECPublicKey) {
		return [33]byte{}, fmt.Errorf("%w: algorithm OID %v is not id-ecPublicKey", ErrMalformedPublicKey, spki.Algorithm.Algorithm)
	}
	if !spki.Algorithm.Parameters.Equal(idSecp256k1) {
		return [33]byte{}, fmt.Errorf("%w: curve OID %v is not secp256k1", ErrMalformedPublicKey, spki.Algorithm.Parameters)
	}

	pub, err := secp256k1.ParsePubKey(spki.PublicKey.Bytes)
	if err != nil {
		return [33]byte{}, fmt.Errorf("%w: parsing EC point: %v", ErrMalformedPublicKey, err)
	}
	var out [33]byte
	copy(out[:], pub.SerializeCompressed())
	return out, nil
}

// marshalDERPublicKey is parseDERPublicKey's inverse -- used by
// FakeKMSClient to produce a realistic DER response the same parsing
// path above must round-trip correctly, and by PrivyKMSClient
// (privy_kms_client.go) to wrap Privy's own raw compressed public key
// into the same shape a real KMS's GetPublicKey response already has.
func marshalDERPublicKey(compressed [33]byte) ([]byte, error) {
	pub, err := secp256k1.ParsePubKey(compressed[:])
	if err != nil {
		return nil, fmt.Errorf("kmssign: marshaling fake public key: %w", err)
	}
	var spki subjectPublicKeyInfo
	spki.Algorithm.Algorithm = idECPublicKey
	spki.Algorithm.Parameters = idSecp256k1
	spki.PublicKey = asn1.BitString{
		Bytes:     pub.SerializeUncompressed(),
		BitLength: len(pub.SerializeUncompressed()) * 8,
	}
	return asn1.Marshal(spki)
}
