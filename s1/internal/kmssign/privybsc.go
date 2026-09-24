// This file is BSCDepositKeys's own real-custody counterpart: instead
// of holding a BIP32 xprv in this process's own memory, it provisions
// and signs through Privy Server Wallets (privy_client.go), a real
// custody-as-a-service backend -- privytron.go's own direct BSC-side
// sibling, mirroring its shape almost exactly except for one real
// difference confirmed live by cmd/privy-probe: an ethereum-typed Privy
// wallet's own creation response (and a separate GET /wallets/{id} call)
// never reports a public_key field at all, only an address. Provision,
// below, closes that gap itself: immediately after creating the wallet,
// it performs ONE throwaway secp256k1_sign call over a fixed,
// never-broadcast digest, recovers the signer's own public key from that
// signature (brute-forcing both ECDSA recovery codes, the same technique
// cmd/privy-probe already used to verify this whole mechanism live), and
// keeps whichever candidate's own derived EVM address matches the
// address Privy itself reported for the wallet -- refusing to record
// unverified custody if neither does. This keeps BSCDepositSigner's
// interface identical in shape to TronDepositSigner (a [33]byte
// compressed public key in, a recoverable signature out), so every layer
// above kmssign (requests.Store, keyRef, every HTTP route) stays exactly
// as unaware of this package's own BSC custody-model choice as it
// already is of the TRON one -- see privytron.go's own top-of-file doc
// comment for why that invisibility is a deliberate, load-bearing
// property, not an accident.
package kmssign

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"

	"github.com/jackc/pgx/v5"

	"s1/internal/db"
)

// ErrBSCDepositKeyNotProvisioned is PublicKey/Sign's own result for an
// index nobody has ever called Provision for -- see
// ErrTronDepositKeyNotProvisioned's own doc comment (privytron.go) for
// why signing for an unprovisioned index is a data-integrity bug, never
// something to silently paper over.
var ErrBSCDepositKeyNotProvisioned = errors.New("kmssign: no Privy BSC deposit key provisioned for that index -- call Provision first")

// ErrBSCDepositPublicKeyRecoveryFailed means Provision's own one-time
// public-key-recovery signature did not recover to a candidate whose
// derived EVM address matches the address Privy itself reported for the
// wallet just created -- a real, serious condition (Privy returned a
// signature from a different wallet than asked, or this process's own
// address derivation disagrees with Privy's), never silently coerced.
// Provision refuses to record custody it cannot itself verify.
var ErrBSCDepositPublicKeyRecoveryFailed = errors.New("kmssign: recovered public key from Privy's own throwaway verification signature does not derive to the EVM address Privy itself reported for this wallet -- refusing to record unverified custody")

// bscDepositPubKeyRecoveryDigest is a FIXED, arbitrary digest signed
// exactly once per BSC deposit wallet, immediately after creation,
// purely to recover that wallet's own real compressed public key --
// never transmitted, broadcast, or used for anything beyond that one
// recovery, and never meant to be secret (fixed, not random, so the
// recovery is reproducible/inspectable).
var bscDepositPubKeyRecoveryDigest = sha256.Sum256([]byte("s1 kmssign: BSC deposit wallet public-key recovery digest -- never broadcast"))

// PrivyBSCKey is one row of s1_privy_deposit_wallets, for chain_type='ethereum'.
type PrivyBSCKey struct {
	Index         uint32
	PrivyWalletID string
	Address       string
	PublicKey     [33]byte
}

// PrivyBSCDepositKeys implements BSCDepositSigner against real Privy
// Server Wallets -- BSCDepositKeys's own drop-in replacement, with
// private key material living inside Privy's own infrastructure instead
// of this process's memory.
type PrivyBSCDepositKeys struct {
	client *privyClient
	pool   *db.Pool
}

// NewPrivyBSCDepositKeys wires a PrivyBSCDepositKeys against pool (the
// same S1 database every other package here already uses) and Privy's
// real API, authenticated with appID/appSecret (PRIVY_APP_ID/
// PRIVY_APP_SECRET) -- the SAME two env vars already used for slot-key
// and TRON-deposit-key Privy signing (one Privy app, independent
// wallets), not a new credential.
func NewPrivyBSCDepositKeys(pool *db.Pool, appID, appSecret string) *PrivyBSCDepositKeys {
	return &PrivyBSCDepositKeys{client: newPrivyClient(appID, appSecret), pool: pool}
}

func scanPrivyBSCKey(row scannableRow) (PrivyBSCKey, error) {
	var k PrivyBSCKey
	var pubKeyBytes []byte
	if err := row.Scan(&k.Index, &k.PrivyWalletID, &k.Address, &pubKeyBytes); err != nil {
		return PrivyBSCKey{}, err
	}
	copy(k.PublicKey[:], pubKeyBytes)
	return k, nil
}

func (k *PrivyBSCDepositKeys) get(ctx context.Context, index uint32) (PrivyBSCKey, error) {
	row := k.pool.QueryRow(ctx, `
		SELECT deposit_index, privy_wallet_id, address, public_key
		FROM s1_privy_deposit_wallets
		WHERE chain_type = 'ethereum' AND deposit_index = $1
	`, index)
	return scanPrivyBSCKey(row)
}

// Provision looks up or creates the real Privy wallet backing BSC
// deposit index -- idempotent: a retried call for the same index always
// returns the SAME wallet, never mints a second one. The ONLY method on
// this type that ever creates custody; PublicKey/Sign below are
// read-only against whatever Provision has already recorded.
func (k *PrivyBSCDepositKeys) Provision(ctx context.Context, index uint32) (PrivyBSCKey, error) {
	existing, err := k.get(ctx, index)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return PrivyBSCKey{}, fmt.Errorf("kmssign: looking up BSC deposit index %d: %w", index, err)
	}

	// Deterministic per index -- same reasoning as privytron.go's own
	// Provision: a genuine concurrent race for the same index sends
	// Privy the SAME idempotency key/external_id, and the INSERT's own
	// unique constraint below is the actual correctness guarantee.
	externalID := fmt.Sprintf("s1-bsc-deposit-%d", index)
	walletID, address, err := k.client.createEVMWallet(ctx, externalID, "s1-bsc-deposit-create-"+externalID)
	if err != nil {
		return PrivyBSCKey{}, fmt.Errorf("kmssign: creating Privy BSC wallet for deposit index %d: %w", index, err)
	}

	// Recover this wallet's own public key -- see this file's own
	// top-of-file doc comment for why this step exists at all. The
	// idempotency key here MUST vary per wallet (index AND walletID),
	// even though the digest itself is a fixed constant across every
	// wallet: otherwise Privy's own idempotency dedup could return one
	// wallet's cached signature for a DIFFERENT wallet's identical
	// recovery request, silently recording the wrong public key.
	recoverIdemKey := fmt.Sprintf("s1-bsc-deposit-pubkey-recovery-%d-%s", index, walletID)
	rBytes, sBytes, err := k.client.secp256k1SignEVM(ctx, walletID, bscDepositPubKeyRecoveryDigest, recoverIdemKey)
	if err != nil {
		return PrivyBSCKey{}, fmt.Errorf("kmssign: recovering public key for Privy BSC wallet %s (deposit index %d): %w", walletID, index, err)
	}
	pubKey, err := recoverPubKeyMatchingAddress(rBytes, sBytes, bscDepositPubKeyRecoveryDigest, address)
	if err != nil {
		return PrivyBSCKey{}, fmt.Errorf("kmssign: BSC deposit index %d, wallet %s: %w", index, walletID, err)
	}

	row := k.pool.QueryRow(ctx, `
		INSERT INTO s1_privy_deposit_wallets (chain_type, deposit_index, privy_wallet_id, address, public_key)
		VALUES ('ethereum', $1, $2, $3, $4)
		ON CONFLICT (chain_type, deposit_index) DO NOTHING
		RETURNING deposit_index, privy_wallet_id, address, public_key
	`, index, walletID, address, pubKey[:])
	inserted, err := scanPrivyBSCKey(row)
	if err == nil {
		return inserted, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return PrivyBSCKey{}, fmt.Errorf("kmssign: recording Privy BSC wallet for deposit index %d: %w", index, err)
	}
	// Lost a race against a concurrent Provision for the same index --
	// the winner's row, not the wallet just (harmlessly) minted above, is
	// what gets returned and used from here on.
	winner, err := k.get(ctx, index)
	if err != nil {
		return PrivyBSCKey{}, fmt.Errorf("kmssign: provisioning index %d: lost the race but couldn't read the winner: %w", index, err)
	}
	return winner, nil
}

// PublicKey implements BSCDepositSigner -- read-only, never provisions.
func (k *PrivyBSCDepositKeys) PublicKey(ctx context.Context, index uint32) ([33]byte, error) {
	key, err := k.get(ctx, index)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return [33]byte{}, ErrBSCDepositKeyNotProvisioned
		}
		return [33]byte{}, fmt.Errorf("kmssign: looking up Privy public key for BSC deposit index %d: %w", index, err)
	}
	return key.PublicKey, nil
}

// Sign implements BSCDepositSigner -- read-only, never provisions (see
// ErrBSCDepositKeyNotProvisioned's own doc comment for why).
func (k *PrivyBSCDepositKeys) Sign(ctx context.Context, index uint32, digest [32]byte, expectedPubKey [33]byte) ([65]byte, error) {
	key, err := k.get(ctx, index)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return [65]byte{}, ErrBSCDepositKeyNotProvisioned
		}
		return [65]byte{}, fmt.Errorf("kmssign: looking up Privy wallet for BSC deposit index %d: %w", index, err)
	}
	// Deterministic per (index, digest) so a retried sign request for the
	// exact same digest is itself safe to repeat against Privy.
	idemKey := fmt.Sprintf("s1-bsc-deposit-sign-%d-%x", index, digest)
	rBytes, sBytes, err := k.client.secp256k1SignEVM(ctx, key.PrivyWalletID, digest, idemKey)
	if err != nil {
		return [65]byte{}, fmt.Errorf("kmssign: signing via Privy for BSC deposit index %d: %w", index, err)
	}
	return finishRecoverableSignatureRS(rBytes, sBytes, digest, expectedPubKey)
}

// deriveEVMAddressForRecovery mirrors s1/internal/slots/evm.go's own
// production DeriveEVMAddress (Keccak256 of the 64-byte uncompressed
// point, last 20 bytes, 0x-prefixed hex) and cmd/privy-probe's own
// identical technique -- duplicated here, not imported, since
// internal/kmssign has zero production dependencies on any sibling
// s1/internal package (dependency_test.go mechanically enforces this:
// it is the only package in this module that may hold, derive, or
// produce anything signing-capable). Deliberately lowercase, not EIP-55
// checksummed: the one caller here (recoverPubKeyMatchingAddress)
// compares case-insensitively against whatever case Privy itself
// reports, never displays this to anyone.
func deriveEVMAddressForRecovery(pub *secp256k1.PublicKey) string {
	uncompressed := pub.SerializeUncompressed()
	h := sha3.NewLegacyKeccak256()
	h.Write(uncompressed[1:])
	hash := h.Sum(nil)
	return "0x" + hex.EncodeToString(hash[len(hash)-20:])
}

// recoverPubKeyMatchingAddress brute-forces both possible recovery codes
// against digest/(rBytes, sBytes) -- the same technique
// finishRecoverableSignatureRS uses internally, but exposed here as its
// own step since Provision needs the recovered PUBLIC KEY itself (to
// store), not a finished 65-byte signature -- and returns whichever
// candidate's own derived EVM address matches reportedAddress. Neither
// matching is ErrBSCDepositPublicKeyRecoveryFailed, a real, serious
// condition never silently coerced.
func recoverPubKeyMatchingAddress(rBytes, sBytes [32]byte, digest [32]byte, reportedAddress string) ([33]byte, error) {
	for recoveryCode := byte(0); recoveryCode <= 1; recoveryCode++ {
		compact := make([]byte, 65)
		compact[0] = compactSigRecoveryBase + recoveryCode + compactSigCompressedFlag
		copy(compact[1:33], rBytes[:])
		copy(compact[33:65], sBytes[:])

		recovered, _, err := ecdsa.RecoverCompact(compact, digest[:])
		if err != nil {
			continue
		}
		if strings.EqualFold(deriveEVMAddressForRecovery(recovered), reportedAddress) {
			var out [33]byte
			copy(out[:], recovered.SerializeCompressed())
			return out, nil
		}
	}
	return [33]byte{}, ErrBSCDepositPublicKeyRecoveryFailed
}
