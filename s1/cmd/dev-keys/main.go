// Command dev-keys prints fresh, random keys for a LOCAL development S1,
// as NAME=value lines:
//
//	S1_KMS_FAKE_SEED           seed for S1_KMS_CLIENT=fake, which derives the treasury slot key
//	S1_BSC_DEPOSIT_XPRV/XPUB   the BIP32 node BSC deposit wallets are derived from
//	S1_TRON_DEPOSIT_XPRV/XPUB  the BIP32 node TRON deposit wallets are derived from
//
// scripts/dev.sh setup runs it once and writes the result into
// s1/.env.local (gitignored). These keys live in a plain file, so they are
// for local development only, never for real money: production keeps
// every key in Privy (S1_KMS_CLIENT=privy, PRIVY_APP_ID/PRIVY_APP_SECRET).
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"fmt"
	"math/big"
	"os"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

var (
	xprvVersion = []byte{0x04, 0x88, 0xAD, 0xE4}
	xpubVersion = []byte{0x04, 0x88, 0xB2, 0x1E}
)

func main() {
	keys, err := generate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "dev-keys:", err)
		os.Exit(1)
	}
	for _, kv := range keys {
		fmt.Printf("%s=%s\n", kv[0], kv[1])
	}
}

// generate returns the NAME/value pairs main prints.
func generate() ([][2]string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	seed := int64(binary.BigEndian.Uint64(b[:]) >> 1) // positive
	bscXprv, bscXpub, err := newMasterNode()
	if err != nil {
		return nil, err
	}
	tronXprv, tronXpub, err := newMasterNode()
	if err != nil {
		return nil, err
	}
	return [][2]string{
		{"S1_KMS_FAKE_SEED", fmt.Sprint(seed)},
		{"S1_BSC_DEPOSIT_XPRV", bscXprv},
		{"S1_BSC_DEPOSIT_XPUB", bscXpub},
		{"S1_TRON_DEPOSIT_XPRV", tronXprv},
		{"S1_TRON_DEPOSIT_XPUB", tronXpub},
	}, nil
}

// newMasterNode makes a BIP32 master node from 32 random bytes and returns
// it serialized as an xprv and its matching xpub.
func newMasterNode() (xprv, xpub string, err error) {
	for {
		var seed [32]byte
		if _, err := rand.Read(seed[:]); err != nil {
			return "", "", err
		}
		mac := hmac.New(sha512.New, []byte("Bitcoin seed"))
		mac.Write(seed[:])
		sum := mac.Sum(nil)
		key, chainCode := sum[:32], sum[32:]

		var scalar secp256k1.ModNScalar
		if overflow := scalar.SetByteSlice(key); overflow || scalar.IsZero() {
			continue // astronomically rare; BIP32 says pick another seed
		}
		priv := secp256k1.NewPrivateKey(&scalar)

		header := make([]byte, 0, 78)
		header = append(header, 0)            // depth
		header = append(header, 0, 0, 0, 0)   // parent fingerprint
		header = append(header, 0, 0, 0, 0)   // child number
		header = append(header, chainCode...) //

		priv78 := append(append(append([]byte{}, xprvVersion...), header...), 0x00)
		priv78 = append(priv78, key...)
		pub78 := append(append([]byte{}, xpubVersion...), header...)
		pub78 = append(pub78, priv.PubKey().SerializeCompressed()...)
		return base58Check(priv78), base58Check(pub78), nil
	}
}

const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func base58Check(payload []byte) string {
	first := sha256.Sum256(payload)
	second := sha256.Sum256(first[:])
	data := append(append([]byte{}, payload...), second[:4]...)

	n := new(big.Int).SetBytes(data)
	radix, mod := big.NewInt(58), new(big.Int)
	var out []byte
	for n.Sign() > 0 {
		n.DivMod(n, radix, mod)
		out = append(out, alphabet[mod.Int64()])
	}
	for _, c := range data {
		if c != 0 {
			break
		}
		out = append(out, alphabet[0])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}
