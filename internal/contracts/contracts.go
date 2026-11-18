// Package contracts encodes calls to and decodes events of the Prismaport EVM contracts
// (Factory, ForwarderImpl, Gateway) and the ERC-20 calls the backend makes.
package contracts

import (
	"fmt"
	"math/big"

	"github.com/prismaport/prismaport/internal/evm"
	"github.com/prismaport/prismaport/internal/evmrpc"
	"github.com/prismaport/prismaport/internal/fee"
	"github.com/prismaport/prismaport/internal/money"
)

// Event topics of the token and gateway contracts, and the function selectors the gateway calls.
var (
	TransferTopic        = evm.Topic("Transfer(address,address,uint256)")
	SettledTopic         = evm.Topic("Settled(address,uint256,uint256,uint256,address)")
	PayoutTopic          = evm.Topic("Payout(address,address,uint256)")
	PaymentReceivedTopic = evm.Topic("PaymentReceived(bytes32,address,address,uint256,uint256,uint256)")

	deployAndFlush = evm.Selector("deployAndFlush(bytes32,(address,uint16,uint64,address[],uint16[],address[]),address[])")
	settle         = evm.Selector("settle((address,address,uint256,uint256,uint256,bytes32,address[],uint16[],uint16,uint64,uint8,bytes32,bytes32))")
	balanceOf      = evm.Selector("balanceOf(address)")
	isBlacklisted  = evm.Selector("isBlacklisted(address)")
	treasury       = evm.Selector("treasury()")
	domainSep      = evm.Selector("DOMAIN_SEPARATOR()")
	name           = evm.Selector("name()")
	authState      = evm.Selector("authorizationState(address,bytes32)")
)

// ForwarderParams are the immutable forwarder arguments (Factory.Params). Recipients and Bps
// are in committed order and of equal length; Tokens are the bound tokens in canonical order
// (derive.EVMTokens), part of the address since D41.
type ForwarderParams struct {
	Treasury   evm.Address
	Fee        fee.Params
	Recipients []evm.Address
	Bps        []uint16
	Tokens     []evm.Address
}

// encodeParams is abi.encode of the Params tuple (dynamic, so its own offsets are relative to
// its start): treasury, feeBps, fixedFeeMicros, the offsets of recipients, bps and tokens, then
// the three arrays.
func encodeParams(p ForwarderParams) []evm.Word {
	n := uint64(len(p.Recipients))
	const head = 6 * 32
	out := []evm.Word{evm.AddressWord(p.Treasury), evm.Uint64Word(uint64(p.Fee.FeeBps)), evm.Uint64Word(p.Fee.FixedFeeMicros),
		evm.Uint64Word(head), evm.Uint64Word(head + 32*(1+n)), evm.Uint64Word(head + 2*32*(1+n))}
	out = append(out, evm.EncodeAddressArray(p.Recipients)...)
	out = append(out, evm.EncodeUint16Array(p.Bps)...)
	return append(out, evm.EncodeAddressArray(p.Tokens)...)
}

// DeployAndFlush encodes Factory.deployAndFlush(salt, params, tokens): params.Tokens are the
// tokens the forwarder binds, tokens the ones to flush now (normally only the session's asset,
// so a paused or frozen token never blocks another).
func DeployAndFlush(salt [32]byte, p ForwarderParams, tokens []evm.Address) []byte {
	params := encodeParams(p)
	head := []evm.Word{evm.Word(salt), evm.Uint64Word(3 * 32), evm.Uint64Word(uint64(3*32 + 32*len(params)))}
	words := append(append(head, params...), evm.EncodeAddressArray(tokens)...)
	return evm.Calldata(deployAndFlush, words...)
}

// Authorization is a signed EIP-3009 ReceiveWithAuthorization plus the session binding the
// Gateway rebuilds the nonce from: the recipients with their shares and the fee.
type Authorization struct {
	Token          evm.Address
	From           evm.Address
	Value          *big.Int
	ValidAfter     *big.Int
	ValidBefore    *big.Int
	SessionSalt    [32]byte
	Recipients     []evm.Address
	Bps            []uint16
	FeeBps         uint16
	FixedFeeMicros uint64
	V              byte
	R, S           evm.Word
}

// Settle encodes Gateway.settle(Authorization): the selector, the tuple's offset word, then the
// tuple with its two arrays after the thirteen head words.
func Settle(a Authorization) []byte {
	n := uint64(len(a.Recipients))
	words := []evm.Word{evm.Uint64Word(32),
		evm.AddressWord(a.Token), evm.AddressWord(a.From), evm.BigWord(a.Value), evm.BigWord(a.ValidAfter),
		evm.BigWord(a.ValidBefore), evm.Word(a.SessionSalt), evm.Uint64Word(13 * 32), evm.Uint64Word(13*32 + 32*(1+n)),
		evm.Uint64Word(uint64(a.FeeBps)), evm.Uint64Word(a.FixedFeeMicros), evm.Uint64Word(uint64(a.V)), a.R, a.S}
	words = append(words, evm.EncodeAddressArray(a.Recipients)...)
	words = append(words, evm.EncodeUint16Array(a.Bps)...)
	return evm.Calldata(settle, words...)
}

// BalanceOf returns the calldata of an ERC-20 balanceOf(holder) call.
func BalanceOf(holder evm.Address) []byte { return evm.Calldata(balanceOf, evm.AddressWord(holder)) }

// forwarderTokens is ForwarderImpl.tokens(), a view every deployed forwarder answers.
var forwarderTokens = evm.Selector("tokens()")

// ForwarderProbe is the calldata of a call to a deposit address that tells whether its forwarder
// is deployed, for a Multicall3 aggregate3 with AllowFailure (D49): a call to an address without
// code succeeds with no return data, a deployed forwarder returns its token list (an ABI array, at
// least 64 bytes), and one deployed from an implementation without tokens() (before D41) reverts.
func ForwarderProbe() []byte { return forwarderTokens[:] }

// ForwarderDeployed reads a ForwarderProbe result. A failed call counts as deployed: a call to an
// address without code never reverts, so the address holds code (a forwarder of an older
// implementation). Reading it as not deployed could miss someone else's flush.
func ForwarderDeployed(r Result) bool { return !r.Success || len(r.Data) > 0 }

// IsBlacklisted encodes FiatToken's isBlacklisted(address) (Circle's USDC).
func IsBlacklisted(account evm.Address) []byte {
	return evm.Calldata(isBlacklisted, evm.AddressWord(account))
}

// AuthorizationState encodes EIP-3009's authorizationState(authorizer, nonce): true once the
// authorization was used or cancelled.
func AuthorizationState(authorizer evm.Address, nonce evm.Word) []byte {
	return evm.Calldata(authState, evm.AddressWord(authorizer), nonce)
}

// SettleAuthorizer returns the token and the payer of Settle calldata.
func SettleAuthorizer(calldata []byte) (token, from evm.Address, ok bool) {
	if len(calldata) < 4+3*32 || [4]byte(calldata[:4]) != settle {
		return token, from, false
	}
	t, okT := evm.Word(calldata[36:68]).Address()
	f, okF := evm.Word(calldata[68:100]).Address()
	return t, f, okT && okF
}

// TreasuryCall returns the calldata of a treasury() call.
func TreasuryCall() []byte { return evm.Calldata(treasury) }

// DomainSeparatorCall returns the calldata of a DOMAIN_SEPARATOR() call, which reads a token's
// EIP-712 domain separator.
func DomainSeparatorCall() []byte { return evm.Calldata(domainSep) }

// NameCall returns the calldata of a name() call.
func NameCall() []byte { return evm.Calldata(name) }

// Uint decodes a single uint256 return value.
func Uint(out []byte) (money.Units, error) {
	if len(out) != 32 {
		return money.Zero, fmt.Errorf("contracts: expected one word, got %d bytes", len(out))
	}
	return money.FromBig(new(big.Int).SetBytes(out))
}

// Bool decodes a single bool return value; anything but a canonical 0 or 1 word is an error.
func Bool(out []byte) (bool, error) {
	if len(out) != 32 {
		return false, fmt.Errorf("contracts: expected one word, got %d bytes", len(out))
	}
	v := new(big.Int).SetBytes(out)
	switch {
	case v.Sign() == 0:
		return false, nil
	case v.Cmp(big.NewInt(1)) == 0:
		return true, nil
	}
	return false, fmt.Errorf("contracts: %x is not a bool", out)
}

// AddressResult decodes a single address return value.
func AddressResult(out []byte) (evm.Address, error) {
	if len(out) != 32 {
		return evm.Address{}, fmt.Errorf("contracts: expected one word, got %d bytes", len(out))
	}
	a, ok := evm.Word(out).Address()
	if !ok {
		return a, fmt.Errorf("contracts: malformed address word")
	}
	return a, nil
}

// StringResult decodes a single ABI string return value.
func StringResult(out []byte) (string, error) {
	if len(out) < 64 {
		return "", fmt.Errorf("contracts: string result too short")
	}
	off := new(big.Int).SetBytes(out[:32])
	if !off.IsUint64() || off.Uint64()+32 > uint64(len(out)) {
		return "", fmt.Errorf("contracts: bad string offset")
	}
	o := off.Uint64()
	n := new(big.Int).SetBytes(out[o : o+32])
	if !n.IsUint64() || o+32+n.Uint64() > uint64(len(out)) {
		return "", fmt.Errorf("contracts: bad string length")
	}
	return string(out[o+32 : o+32+n.Uint64()]), nil
}

func amounts(words []evm.Word) ([]money.Units, bool) {
	out := make([]money.Units, len(words))
	for i, w := range words {
		u, err := money.FromBig(w.Big())
		if err != nil {
			return nil, false
		}
		out[i] = u
	}
	return out, true
}

// Settled is a forwarder's Settled(token indexed, gross, fee, net, treasury) event.
type Settled struct {
	Forwarder evm.Address
	Token     evm.Address
	Treasury  evm.Address
	Gross     money.Units
	Fee       money.Units
	Net       money.Units
	TxHash    evm.Word
	LogIndex  uint32
}

// ParseSettled decodes a Settled log; ok is false for any other log. The emitter is not
// checked here: callers accept only forwarders they derived.
func ParseSettled(l evmrpc.Log) (Settled, bool) {
	if len(l.Topics) != 2 || l.Topics[0] != SettledTopic {
		return Settled{}, false
	}
	words, err := evm.Words(l.Data)
	if err != nil || len(words) != 4 {
		return Settled{}, false
	}
	s := Settled{Forwarder: l.Address, TxHash: l.TxHash, LogIndex: l.LogIndex}
	var ok1, ok2 bool
	s.Token, ok1 = l.Topics[1].Address()
	s.Treasury, ok2 = words[3].Address()
	us, ok3 := amounts(words[:3])
	if !ok1 || !ok2 || !ok3 {
		return Settled{}, false
	}
	s.Gross, s.Fee, s.Net = us[0], us[1], us[2]
	return s, true
}

// Payout is one Payout(token indexed, recipient indexed, amount) event, emitted by a forwarder
// or by the Gateway for every non-zero share. The same signature exists on both contracts and
// anyone can emit it, so callers filter by emitter.
type Payout struct {
	Emitter   evm.Address
	Token     evm.Address
	Recipient evm.Address
	Amount    money.Units
	TxHash    evm.Word
	LogIndex  uint32
}

// ParsePayout decodes a Payout event log, reporting false when the log is not one.
func ParsePayout(l evmrpc.Log) (Payout, bool) {
	if len(l.Topics) != 3 || l.Topics[0] != PayoutTopic {
		return Payout{}, false
	}
	words, err := evm.Words(l.Data)
	if err != nil || len(words) != 1 {
		return Payout{}, false
	}
	p := Payout{Emitter: l.Address, TxHash: l.TxHash, LogIndex: l.LogIndex}
	var ok1, ok2 bool
	p.Token, ok1 = l.Topics[1].Address()
	p.Recipient, ok2 = l.Topics[2].Address()
	us, ok3 := amounts(words)
	if !ok1 || !ok2 || !ok3 {
		return Payout{}, false
	}
	p.Amount = us[0]
	return p, true
}

// PaymentReceived is the Gateway's PaymentReceived(nonce, token, from indexed; gross, fee, net).
type PaymentReceived struct {
	Gateway  evm.Address
	Nonce    evm.Word
	Token    evm.Address
	From     evm.Address
	Gross    money.Units
	Fee      money.Units
	Net      money.Units
	TxHash   evm.Word
	LogIndex uint32
}

// ParsePaymentReceived decodes a PaymentReceived event log, reporting false when the log is not
// one.
func ParsePaymentReceived(l evmrpc.Log) (PaymentReceived, bool) {
	if len(l.Topics) != 4 || l.Topics[0] != PaymentReceivedTopic {
		return PaymentReceived{}, false
	}
	words, err := evm.Words(l.Data)
	if err != nil || len(words) != 3 {
		return PaymentReceived{}, false
	}
	p := PaymentReceived{Gateway: l.Address, Nonce: l.Topics[1], TxHash: l.TxHash, LogIndex: l.LogIndex}
	var ok1, ok2 bool
	p.Token, ok1 = l.Topics[2].Address()
	p.From, ok2 = l.Topics[3].Address()
	us, ok3 := amounts(words)
	if !ok1 || !ok2 || !ok3 {
		return PaymentReceived{}, false
	}
	p.Gross, p.Fee, p.Net = us[0], us[1], us[2]
	return p, true
}

// PayoutsBefore collects, for one settlement event at logIndex in a transaction, the Payout
// events the same emitter logged for the same token earlier in that transaction and after any
// earlier settlement event of that emitter and token (bounds holds their log indexes). The
// result is in log order: recipient order, zero shares skipped.
func PayoutsBefore(logs []evmrpc.Log, emitter, token evm.Address, tx evm.Word, logIndex uint32, bounds []uint32) []Payout {
	lower := uint32(0)
	hasLower := false
	for _, b := range bounds {
		if b < logIndex && (!hasLower || b > lower) {
			lower, hasLower = b, true
		}
	}
	var out []Payout
	for _, l := range logs {
		if l.Address != emitter || l.TxHash != tx || l.LogIndex >= logIndex || (hasLower && l.LogIndex <= lower) {
			continue
		}
		p, ok := ParsePayout(l)
		if !ok || p.Token != token {
			continue
		}
		out = append(out, p)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].LogIndex < out[j-1].LogIndex; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// MatchPayouts places observed Payout events on a recipient list: one amount per recipient,
// zero where no event was logged (a zero share is skipped on chain). ok is false when an event
// names an address outside the list, repeats a recipient, or breaks the committed order.
func MatchPayouts(recipients []evm.Address, events []Payout) ([]money.Units, bool) {
	out := make([]money.Units, len(recipients))
	for i := range out {
		out[i] = money.Zero
	}
	next := 0
	for _, e := range events {
		found := false
		for i := next; i < len(recipients); i++ {
			if recipients[i] == e.Recipient {
				out[i], next, found = e.Amount, i+1, true
				break
			}
		}
		if !found || e.Amount.IsZero() {
			return nil, false
		}
	}
	return out, true
}

// RevertReason decodes Error(string) revert data, or names a known custom error.
func RevertReason(data []byte) string {
	if len(data) < 4 {
		return "reverted without a reason"
	}
	sel := [4]byte(data[:4])
	if sel == evm.Selector("Error(string)") {
		if s, err := StringResult(data[4:]); err == nil {
			return s
		}
	}
	for _, e := range []string{"DustPayment()", "ZeroAddress()", "ReceivedAmountMismatch(uint256,uint256)", "NotAClone()",
		"InvalidRecipient(uint256)", "InvalidRecipientCount(uint256)", "LengthMismatch(uint256,uint256)", "ZeroRecipient(uint256)",
		"DuplicateRecipient(uint256)", "ZeroShare(uint256)", "InvalidShareSum(uint256)", "FeeBpsTooHigh(uint16)",
		"FixedFeeTooHigh(uint64)", "UnsupportedDecimals(uint8)", "InvalidTokenCount(uint256)", "ZeroToken(uint256)",
		"TokensNotAscending(uint256)", "TokenNotAllowed(address)", "NotTreasury()", "TokenIsBound(address)"} {
		if sel == evm.Selector(e) {
			return e
		}
	}
	return fmt.Sprintf("reverted with 0x%x", data[:4])
}
