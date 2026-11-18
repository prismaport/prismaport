package evm

import "math/big"

var (
	domainTypeHash = Keccak256([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
	// ReceiveWithAuthorizationType is the EIP-3009 struct type FiatToken verifies.
	ReceiveWithAuthorizationType = "ReceiveWithAuthorization(address from,address to,uint256 value,uint256 validAfter,uint256 validBefore,bytes32 nonce)"
	receiveTypeHash              = Keccak256([]byte(ReceiveWithAuthorizationType))
)

// Domain is an EIP-712 domain with the four fields FiatToken uses.
type Domain struct {
	Name              string
	Version           string
	ChainID           uint64
	VerifyingContract Address
}

// Separator returns the EIP-712 domain separator of the domain.
func (d Domain) Separator() Word {
	var w Word
	nameHash, versionHash := Keccak256([]byte(d.Name)), Keccak256([]byte(d.Version))
	cid, vc := Uint64Word(d.ChainID), AddressWord(d.VerifyingContract)
	copy(w[:], Keccak256(domainTypeHash, nameHash, versionHash, cid[:], vc[:]))
	return w
}

// ReceiveAuthorization is the message a payer signs for the signed payment path.
type ReceiveAuthorization struct {
	From        Address
	To          Address
	Value       *big.Int
	ValidAfter  *big.Int
	ValidBefore *big.Int
	Nonce       Word
}

// Digest is keccak256(0x1901 ++ domainSeparator ++ structHash).
func (a ReceiveAuthorization) Digest(d Domain) Word {
	from, to := AddressWord(a.From), AddressWord(a.To)
	value, after, before := BigWord(a.Value), BigWord(a.ValidAfter), BigWord(a.ValidBefore)
	structHash := Keccak256(receiveTypeHash, from[:], to[:], value[:], after[:], before[:], a.Nonce[:])
	sep := d.Separator()
	var w Word
	copy(w[:], Keccak256([]byte{0x19, 0x01}, sep[:], structHash))
	return w
}

// GatewayNonce reproduces Gateway.computeNonce:
// keccak256(abi.encode(chainid, gateway, sessionSalt, treasury, feeBps, fixedFeeMicros, recipients, bps)),
// with the two dynamic arrays encoded after the six static words (see protocol.md, Gateway).
func GatewayNonce(chainID uint64, gateway Address, sessionSalt [32]byte, treasury Address, feeBps uint16, fixedFeeMicros uint64,
	recipients []Address, bps []uint16) Word {
	n := uint64(len(recipients))
	parts := []Word{Uint64Word(chainID), AddressWord(gateway), Word(sessionSalt), AddressWord(treasury),
		Uint64Word(uint64(feeBps)), Uint64Word(fixedFeeMicros), Uint64Word(8 * 32), Uint64Word(9*32 + 32*n)}
	parts = append(parts, EncodeAddressArray(recipients)...)
	parts = append(parts, EncodeUint16Array(bps)...)
	var buf []byte
	for _, p := range parts {
		buf = append(buf, p[:]...)
	}
	var w Word
	copy(w[:], Keccak256(buf))
	return w
}
