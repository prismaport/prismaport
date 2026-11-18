package solana

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/prismaport/prismaport/internal/solanaderive"
)

// AccountSpec is one account of a forwarder instruction as the IDL declares it.
type AccountSpec struct {
	Name     string
	Writable bool
	Signer   bool
	// Fixed is the address the IDL pins the account to, if any.
	Fixed string
}

// InstructionSpec is one instruction of the prismaport_forwarder IDL: the Anchor discriminator
// (sha256("global:<name>")[:8]) and the accounts in order. A test checks this table against
// solana/idl/prismaport_forwarder.json.
type InstructionSpec struct {
	Name          string
	Discriminator [8]byte
	Accounts      []AccountSpec
}

// The program is stateless (D41): no instruction creates or closes an account at the forwarder
// address. Both instructions take salt ++ Borsh(params) as data, and the seeds constraint proves
// they are the ones the address commits to.
var (
	// IxFlush's fixed accounts; the recipients' token accounts follow as remaining accounts,
	// writable, in recipient order (Forwarder.Flush appends them). Data: salt ++ params ++ bool
	// close_source.
	IxFlush = InstructionSpec{"flush", [8]byte{204, 128, 70, 211, 133, 144, 249, 130}, []AccountSpec{
		{Name: "caller", Writable: true, Signer: true}, {Name: "forwarder"}, {Name: "mint"}, {Name: "source", Writable: true},
		{Name: "treasury"}, {Name: "treasury_ata", Writable: true}, {Name: "rent_receiver", Writable: true},
		{Name: "token_program", Fixed: "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"},
		{Name: "associated_token_program", Fixed: "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"},
		{Name: "system_program", Fixed: "11111111111111111111111111111111"}}}
	// IxCloseTokenAccount closes an empty token account of a bound mint, rent to the rent
	// receiver. Data: salt ++ params.
	IxCloseTokenAccount = InstructionSpec{"close_token_account", [8]byte{132, 172, 24, 60, 100, 156, 135, 97}, []AccountSpec{
		{Name: "forwarder"}, {Name: "token_account", Writable: true}, {Name: "rent_receiver", Writable: true},
		{Name: "token_program", Fixed: "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"}}}

	ForwarderInstructions = []InstructionSpec{IxFlush, IxCloseTokenAccount}

	// SettledEventDiscriminator is sha256("event:Settled")[:8].
	SettledEventDiscriminator = [8]byte{0xe8, 0xd2, 0x28, 0x11, 0x8e, 0x7c, 0x91, 0xee}
	// PayoutEventDiscriminator is sha256("event:Payout")[:8].
	PayoutEventDiscriminator = [8]byte{0x9c, 0xf7, 0x22, 0x19, 0x06, 0xb1, 0x4b, 0x0f}
)

func (s InstructionSpec) build(program PublicKey, keys []PublicKey, data []byte) Instruction {
	ix := Instruction{Program: program, Data: append(s.Discriminator[:], data...)}
	for i, a := range s.Accounts {
		ix.Accounts = append(ix.Accounts, AccountMeta{Key: keys[i], Signer: a.Signer, Writable: a.Writable})
	}
	return ix
}

// ForwarderParams are the committed forwarder values.
type ForwarderParams = solanaderive.Params

// Forwarder addresses one deposit address: the program, the PDA and what it commits to.
type Forwarder struct {
	Program PublicKey
	Address PublicKey
	Salt    [32]byte
	Params  ForwarderParams
}

// NewForwarder derives the PDA; it refuses params the program would reject.
func NewForwarder(program PublicKey, salt [32]byte, p ForwarderParams) (Forwarder, error) {
	pda, _, err := solanaderive.ForwarderAddress(program, salt, p)
	if err != nil {
		return Forwarder{}, err
	}
	return Forwarder{Program: program, Address: pda, Salt: salt, Params: p}, nil
}

// ixArgs is salt ++ Borsh(params), the leading arguments of both instructions.
func (f Forwarder) ixArgs() []byte {
	return append(append([]byte(nil), f.Salt[:]...), f.Params.Encode()...)
}

// FlushData is flush's instruction data: discriminator ++ salt ++ params ++ close_source.
func (f Forwarder) FlushData(closeSource bool) []byte {
	b := byte(0)
	if closeSource {
		b = 1
	}
	return append(append(IxFlush.Discriminator[:], f.ixArgs()...), b)
}

// RecipientATAs are the recipients' classic token accounts of mint, in recipient order: the
// accounts flush pays the shares into. The program never creates them.
func (f Forwarder) RecipientATAs(mint PublicKey) ([]PublicKey, error) {
	out := make([]PublicKey, len(f.Params.Recipients))
	for i, r := range f.Params.Recipients {
		a, err := AssociatedTokenAddress(r.Wallet, mint)
		if err != nil {
			return nil, err
		}
		out[i] = a
	}
	return out, nil
}

// Flush settles the whole balance of source: the fee into the treasury's ATA and each share
// into its recipient's ATA, passed as remaining accounts in recipient order. With closeSource
// the program then closes source, rent to the rent receiver. A mint the forwarder does not bind
// is refused here, as the program would (MintNotAllowed).
func (f Forwarder) Flush(caller, mint, source PublicKey, closeSource bool) (Instruction, error) {
	if !f.Params.Allows(mint) {
		return Instruction{}, fmt.Errorf("%w: %s", solanaderive.ErrMintNotAllowed, mint)
	}
	treATA, err := AssociatedTokenAddress(f.Params.Treasury, mint)
	if err != nil {
		return Instruction{}, err
	}
	atas, err := f.RecipientATAs(mint)
	if err != nil {
		return Instruction{}, err
	}
	ix := IxFlush.build(f.Program, []PublicKey{caller, f.Address, mint, source, f.Params.Treasury, treATA, f.Params.RentReceiver,
		TokenProgramID, AssociatedTokenProgramID, SystemProgramID}, nil)
	ix.Data = f.FlushData(closeSource)
	for _, a := range atas {
		ix.Accounts = append(ix.Accounts, W(a))
	}
	return ix, nil
}

// CloseTokenAccount closes an empty token account the forwarder owns, rent to the rent
// receiver. The program refuses it unless the account's mint is bound and its balance is zero.
func (f Forwarder) CloseTokenAccount(account PublicKey) Instruction {
	return IxCloseTokenAccount.build(f.Program, []PublicKey{f.Address, account, f.Params.RentReceiver, TokenProgramID}, f.ixArgs())
}

// SettledEvent is the forwarder's Settled event (Anchor emit!, Borsh after the discriminator).
type SettledEvent struct {
	Forwarder, Mint, Source PublicKey
	Gross, Fee, Net         uint64
	Treasury                PublicKey
}

// PayoutEvent is one Payout event: a non-zero share paid to a recipient's token account.
type PayoutEvent struct {
	Forwarder, Mint, Recipient PublicKey
	Amount                     uint64
}

const (
	settledLen = 8 + 32*3 + 8*3 + 32
	payoutLen  = 8 + 32*3 + 8
)

type borsh struct {
	b []byte
	p int
}

func (r *borsh) key() PublicKey { k := PublicKey(r.b[r.p : r.p+32]); r.p += 32; return k }
func (r *borsh) u64() uint64    { v := binary.LittleEndian.Uint64(r.b[r.p:]); r.p += 8; return v }

// DecodeSettled parses the data of a forwarder Settled event and reports false when the length or discriminator does not match.
func DecodeSettled(b []byte) (SettledEvent, bool) {
	var e SettledEvent
	if len(b) != settledLen || [8]byte(b[:8]) != SettledEventDiscriminator {
		return e, false
	}
	r := &borsh{b: b, p: 8}
	e.Forwarder, e.Mint, e.Source = r.key(), r.key(), r.key()
	e.Gross, e.Fee, e.Net = r.u64(), r.u64(), r.u64()
	e.Treasury = r.key()
	return e, true
}

// DecodePayout parses the data of a forwarder Payout event and reports false when the length or discriminator does not match.
func DecodePayout(b []byte) (PayoutEvent, bool) {
	var e PayoutEvent
	if len(b) != payoutLen || [8]byte(b[:8]) != PayoutEventDiscriminator {
		return e, false
	}
	r := &borsh{b: b, p: 8}
	e.Forwarder, e.Mint, e.Recipient = r.key(), r.key(), r.key()
	e.Amount = r.u64()
	return e, true
}

func programData(logs []string) [][]byte {
	var out [][]byte
	for _, l := range logs {
		rest, ok := strings.CutPrefix(l, "Program data: ")
		if !ok {
			continue
		}
		if b, err := base64.StdEncoding.DecodeString(rest); err == nil {
			out = append(out, b)
		}
	}
	return out
}

// SettledEvents extracts Settled events from program logs. Logs are not authenticated (any
// program can print the same line), so callers only use them to cross-check balance changes.
func SettledEvents(logs []string) []SettledEvent {
	var out []SettledEvent
	for _, b := range programData(logs) {
		if e, ok := DecodeSettled(b); ok {
			out = append(out, e)
		}
	}
	return out
}

// PayoutEvents extracts Payout events from program logs, in log order; like SettledEvents they
// are only a cross-check.
func PayoutEvents(logs []string) []PayoutEvent {
	var out []PayoutEvent
	for _, b := range programData(logs) {
		if e, ok := DecodePayout(b); ok {
			out = append(out, e)
		}
	}
	return out
}

// SessionReference is the read-only key a connected-wallet payment carries so the detector can
// find it with getSignaturesForAddress. It is derived from the session salt, so it never needs
// storing; any 32 bytes are a valid account key for this purpose.
func SessionReference(salt [32]byte) PublicKey {
	return PublicKey(sha256.Sum256(append([]byte("prismaport:solana:reference:v1"), salt[:]...)))
}
