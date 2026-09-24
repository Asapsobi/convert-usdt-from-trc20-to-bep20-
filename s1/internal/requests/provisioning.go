// This file is S1's own key-PROVISIONING surface -- deliberately
// separate from store.go's own signing-request code, since provisioning
// a deposit key (mint or look up the real custody backing one index)
// has a genuinely different lifecycle than requesting a signature: no
// PENDING/SIGNED, no approval threshold, no idempotency-key request
// field (the deposit index's own uniqueness already is the idempotency
// key). Not part of SigningService either -- that interface's own doc
// comment scopes it to "the one thing C5 calls," and the caller of this
// is tronwatcher, never C5.
package requests

import (
	"context"
	"errors"
)

// TronDepositKey is ProvisionTronDepositKey's own result -- just enough
// for a caller (tronwatcher, via S1's own HTTP boundary) to record a
// real deposit address, never anything private.
type TronDepositKey struct {
	Index   uint32
	Address string
}

// TronDepositProvisioner is the one call this package needs from
// internal/kmssign to provision a real, custody-backed TRON deposit key
// -- kmssign.PrivyTronDepositKeys's own Provision method, or a fake for
// testing.
type TronDepositProvisioner interface {
	Provision(ctx context.Context, index uint32) (address string, publicKey [33]byte, err error)
}

// ErrTronDepositProvisioningNotConfigured is ProvisionTronDepositKey's
// own result on a Store built with a nil TronDepositProvisioner -- this
// capability is opt-in (only meaningful for a deployment that has
// configured Privy-backed TRON deposit signing), never assumed.
var ErrTronDepositProvisioningNotConfigured = errors.New("requests: Privy-backed TRON deposit-key provisioning is not configured on this S1 deployment")

// ProvisionTronDepositKey looks up or creates the real custody backing
// TRON deposit index -- idempotent: a retried call for the same index
// always returns the same address, never mints a second key. This never
// touches signing_requests/signing_audit_log at all (see this file's own
// top-of-file doc comment for why provisioning is a separate concern
// from every other method in this package).
func (s *Store) ProvisionTronDepositKey(ctx context.Context, index uint32) (TronDepositKey, error) {
	if s.tronDepositProvisioner == nil {
		return TronDepositKey{}, ErrTronDepositProvisioningNotConfigured
	}
	address, _, err := s.tronDepositProvisioner.Provision(ctx, index)
	if err != nil {
		return TronDepositKey{}, err
	}
	return TronDepositKey{Index: index, Address: address}, nil
}

// BSCDepositKey is ProvisionBSCDepositKey's own result -- see
// TronDepositKey's own doc comment.
type BSCDepositKey struct {
	Index   uint32
	Address string
}

// BSCDepositProvisioner is TronDepositProvisioner's own BSC-deposit
// counterpart -- kmssign.PrivyBSCDepositKeys's own Provision method, or
// a fake for testing.
type BSCDepositProvisioner interface {
	Provision(ctx context.Context, index uint32) (address string, publicKey [33]byte, err error)
}

// ErrBSCDepositProvisioningNotConfigured is ErrTronDepositProvisioningNotConfigured's
// own BSC-deposit counterpart.
var ErrBSCDepositProvisioningNotConfigured = errors.New("requests: Privy-backed BSC deposit-key provisioning is not configured on this S1 deployment")

// ProvisionBSCDepositKey is ProvisionTronDepositKey's own BSC-deposit
// counterpart -- see that method's own doc comment.
func (s *Store) ProvisionBSCDepositKey(ctx context.Context, index uint32) (BSCDepositKey, error) {
	if s.bscDepositProvisioner == nil {
		return BSCDepositKey{}, ErrBSCDepositProvisioningNotConfigured
	}
	address, _, err := s.bscDepositProvisioner.Provision(ctx, index)
	if err != nil {
		return BSCDepositKey{}, err
	}
	return BSCDepositKey{Index: index, Address: address}, nil
}
