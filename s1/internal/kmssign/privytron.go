// This file is TronDepositKeys's own real-custody counterpart: instead
// of holding a BIP32 xprv in this process's own memory, it provisions
// and signs through Privy Server Wallets (privy_client.go), a real
// custody-as-a-service backend. See that file's own top-of-file doc
// comment for exactly which facts here were empirically proven against
// Privy's real API by cmd/privy-probe, not guessed from documentation.
//
// This file gives internal/kmssign a *db.Pool dependency for the first
// time -- a deliberate boundary expansion, not scope creep: a Privy
// wallet is signing-capable custody the instant it's minted, so the
// registry recording which deposit index maps to which live,
// funds-controlling Privy wallet is exactly as custody-sensitive as
// internal/slots' own s1_slot_keys table, and belongs in the one package
// this module's own doc comment says may hold or produce anything
// signing-capable.
package kmssign

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"s1/internal/db"
)

// ErrTronDepositKeyNotProvisioned is PublicKey/Sign's own result for an
// index nobody has ever called Provision for -- signing for an
// unprovisioned index is a data-integrity bug (the caller should have
// provisioned it at address-issuance time, long before any signature is
// ever requested), never something to silently paper over by minting a
// wallet mid-signature-request.
var ErrTronDepositKeyNotProvisioned = errors.New("kmssign: no Privy TRON deposit key provisioned for that index -- call Provision first")

// PrivyTronKey is one row of s1_privy_deposit_wallets, for chain_type='tron'.
type PrivyTronKey struct {
	Index         uint32
	PrivyWalletID string
	Address       string
	PublicKey     [33]byte
}

// PrivyTronDepositKeys implements TronDepositSigner against real Privy
// Server Wallets -- TronDepositKeys's own drop-in replacement, with
// private key material living inside Privy's own infrastructure instead
// of this process's memory.
type PrivyTronDepositKeys struct {
	client *privyClient
	pool   *db.Pool
}

// NewPrivyTronDepositKeys wires a PrivyTronDepositKeys against pool (the
// same S1 database every other package here already uses) and Privy's
// real API, authenticated with appID/appSecret (PRIVY_APP_ID/
// PRIVY_APP_SECRET).
func NewPrivyTronDepositKeys(pool *db.Pool, appID, appSecret string) *PrivyTronDepositKeys {
	return &PrivyTronDepositKeys{client: newPrivyClient(appID, appSecret), pool: pool}
}

type scannableRow interface {
	Scan(dest ...any) error
}

func scanPrivyTronKey(row scannableRow) (PrivyTronKey, error) {
	var k PrivyTronKey
	var pubKeyBytes []byte
	if err := row.Scan(&k.Index, &k.PrivyWalletID, &k.Address, &pubKeyBytes); err != nil {
		return PrivyTronKey{}, err
	}
	copy(k.PublicKey[:], pubKeyBytes)
	return k, nil
}

func (k *PrivyTronDepositKeys) get(ctx context.Context, index uint32) (PrivyTronKey, error) {
	row := k.pool.QueryRow(ctx, `
		SELECT deposit_index, privy_wallet_id, address, public_key
		FROM s1_privy_deposit_wallets
		WHERE chain_type = 'tron' AND deposit_index = $1
	`, index)
	return scanPrivyTronKey(row)
}

// Provision looks up or creates the real Privy wallet backing TRON
// deposit index -- idempotent: a retried call for the same index always
// returns the SAME wallet, never mints a second one. The ONLY method on
// this type that ever creates custody; PublicKey/Sign below are
// read-only against whatever Provision has already recorded.
func (k *PrivyTronDepositKeys) Provision(ctx context.Context, index uint32) (PrivyTronKey, error) {
	existing, err := k.get(ctx, index)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return PrivyTronKey{}, fmt.Errorf("kmssign: looking up TRON deposit index %d: %w", index, err)
	}

	// Deterministic per index -- even a genuine concurrent race for the
	// same index sends Privy the SAME idempotency key/external_id. The
	// INSERT's own unique constraint below is still the actual
	// correctness guarantee, not reliance on Privy's own dedup: a
	// Privy-side dedup miss only orphans a harmless extra wallet (real,
	// effectively-zero-cost, confirmed by cmd/privy-probe), never a
	// correctness bug.
	externalID := fmt.Sprintf("s1-tron-deposit-%d", index)
	walletID, address, pubKey, err := k.client.createTronWallet(ctx, externalID, "s1-tron-deposit-create-"+externalID)
	if err != nil {
		return PrivyTronKey{}, fmt.Errorf("kmssign: creating Privy TRON wallet for deposit index %d: %w", index, err)
	}

	row := k.pool.QueryRow(ctx, `
		INSERT INTO s1_privy_deposit_wallets (chain_type, deposit_index, privy_wallet_id, address, public_key)
		VALUES ('tron', $1, $2, $3, $4)
		ON CONFLICT (chain_type, deposit_index) DO NOTHING
		RETURNING deposit_index, privy_wallet_id, address, public_key
	`, index, walletID, address, pubKey[:])
	inserted, err := scanPrivyTronKey(row)
	if err == nil {
		return inserted, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return PrivyTronKey{}, fmt.Errorf("kmssign: recording Privy TRON wallet for deposit index %d: %w", index, err)
	}
	// Lost a race against a concurrent Provision for the same index --
	// the winner's row, not the wallet just (harmlessly) minted above, is
	// what gets returned and used from here on.
	winner, err := k.get(ctx, index)
	if err != nil {
		return PrivyTronKey{}, fmt.Errorf("kmssign: provisioning index %d: lost the race but couldn't read the winner: %w", index, err)
	}
	return winner, nil
}

// PublicKey implements TronDepositSigner -- read-only, never provisions.
func (k *PrivyTronDepositKeys) PublicKey(ctx context.Context, index uint32) ([33]byte, error) {
	key, err := k.get(ctx, index)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return [33]byte{}, ErrTronDepositKeyNotProvisioned
		}
		return [33]byte{}, fmt.Errorf("kmssign: looking up Privy public key for TRON deposit index %d: %w", index, err)
	}
	return key.PublicKey, nil
}

// Sign implements TronDepositSigner -- read-only, never provisions (see
// ErrTronDepositKeyNotProvisioned's own doc comment for why).
func (k *PrivyTronDepositKeys) Sign(ctx context.Context, index uint32, digest [32]byte, expectedPubKey [33]byte) ([65]byte, error) {
	key, err := k.get(ctx, index)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return [65]byte{}, ErrTronDepositKeyNotProvisioned
		}
		return [65]byte{}, fmt.Errorf("kmssign: looking up Privy wallet for TRON deposit index %d: %w", index, err)
	}
	// Deterministic per (index, digest) so a retried sign request for the
	// exact same digest is itself safe to repeat against Privy.
	idemKey := fmt.Sprintf("s1-tron-deposit-sign-%d-%x", index, digest)
	rBytes, sBytes, err := k.client.rawSignTron(ctx, key.PrivyWalletID, digest, idemKey)
	if err != nil {
		return [65]byte{}, fmt.Errorf("kmssign: signing via Privy for TRON deposit index %d: %w", index, err)
	}
	return finishRecoverableSignatureRS(rBytes, sBytes, digest, expectedPubKey)
}
