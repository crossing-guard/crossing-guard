package engine

import (
	"crypto/rand"
	"crypto/sha256"
	"math/big"
	"regexp"
	"time"
)

// Typed ids are the wire identity of portable records (schemas/common.schema.json
// `typedId`): a three-letter kind, an underscore, then 10–32 Crockford base32 chars.
// The local store keeps its integer ids; these exist so two devices can never mint
// the same id and a server can dedupe by id + content hash. The minter lives in the
// portable core because the server mints ids with the same code.
//
// The spool's hex `obs_`/`act_` ids (guardcli.newRecordID) are a different contract —
// local correlation, never pushed — and are deliberately not changed here.

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // 32 symbols; I, L, O, U excluded

var typedIDPattern = regexp.MustCompile(`^[a-z]{3}_[0-9A-HJKMNP-TV-Z]{10,32}$`)

// IsTypedID reports whether s has the wire shape.
func IsTypedID(s string) bool { return typedIDPattern.MatchString(s) }

// NewTypedID mints a ULID-shaped id: 48 bits of millisecond time then 80 random bits,
// encoded as 26 Crockford characters. Time-ordered within a device, unique across them.
func NewTypedID(prefix string) string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	if _, err := rand.Read(b[6:]); err != nil {
		// Randomness failure is not a reason to hand out a colliding id; fall back to a
		// hash of the clock, which is at least distinct per call on a monotonic system.
		h := sha256.Sum256([]byte(time.Now().String()))
		copy(b[6:], h[:10])
	}
	return prefix + "_" + encode128(b)
}

// DeterministicTypedID derives a stable id from material — used to backfill legacy rows
// so that migrating the same store twice yields the same ids. Same shape as NewTypedID.
func DeterministicTypedID(prefix, material string) string {
	h := sha256.Sum256([]byte(material))
	var b [16]byte
	copy(b[:], h[:16])
	return prefix + "_" + encode128(b)
}

// encode128 renders a big-endian 128-bit value as 26 Crockford base32 characters
// (130 bits of capacity, so the leading character is 0–7 as in a ULID).
func encode128(b [16]byte) string {
	n := new(big.Int).SetBytes(b[:])
	out := make([]byte, 26)
	thirtyTwo := big.NewInt(32)
	mod := new(big.Int)
	for i := 25; i >= 0; i-- {
		n.DivMod(n, thirtyTwo, mod)
		out[i] = crockford[mod.Int64()]
	}
	return string(out)
}
