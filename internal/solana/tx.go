package solana

import (
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/prismaport/prismaport/internal/base58"
)

// AccountMeta is one account an instruction touches.
type AccountMeta struct {
	Key      PublicKey
	Signer   bool
	Writable bool
}

// W returns a writable, non-signer account meta.
func W(k PublicKey) AccountMeta { return AccountMeta{Key: k, Writable: true} }

// R returns a read-only, non-signer account meta.
func R(k PublicKey) AccountMeta { return AccountMeta{Key: k} }

// WS returns a writable signer account meta.
func WS(k PublicKey) AccountMeta { return AccountMeta{Key: k, Writable: true, Signer: true} }

// Instruction is a program call before compilation: its program, accounts and data.
type Instruction struct {
	Program  PublicKey
	Accounts []AccountMeta
	Data     []byte
}

// CompiledInstruction is an instruction whose program and accounts are indexes into the message's
// account keys.
type CompiledInstruction struct {
	ProgramIndex uint8
	Accounts     []uint8
	Data         []byte
}

// Header counts the message's signers and read-only accounts, which fixes each account's role by
// position.
type Header struct {
	RequiredSignatures uint8
	ReadonlySigned     uint8
	ReadonlyUnsigned   uint8
}

// Message is a legacy (unversioned) transaction message.
type Message struct {
	Header          Header
	AccountKeys     []PublicKey
	RecentBlockhash [32]byte
	Instructions    []CompiledInstruction
}

// ErrMessage is returned when a message or transaction is malformed.
var ErrMessage = errors.New("solana: malformed message")

// maxPacket is the network limit for a serialized transaction.
const maxPacket = 1232

// Compile orders accounts as the runtime requires (writable signers with the fee payer first,
// readonly signers, writable non-signers, readonly non-signers), merging duplicates.
func Compile(feePayer PublicKey, blockhash [32]byte, ixs []Instruction) (Message, error) {
	type meta struct {
		signer, writable bool
		order            int
	}
	metas := map[PublicKey]*meta{feePayer: {signer: true, writable: true}}
	order := []PublicKey{feePayer}
	add := func(k PublicKey, signer, writable bool) {
		m, ok := metas[k]
		if !ok {
			m = &meta{order: len(order)}
			metas[k] = m
			order = append(order, k)
		}
		m.signer = m.signer || signer
		m.writable = m.writable || writable
	}
	for _, ix := range ixs {
		for _, a := range ix.Accounts {
			add(a.Key, a.Signer, a.Writable)
		}
		add(ix.Program, false, false)
	}
	var groups [4][]PublicKey
	for _, k := range order {
		m := metas[k]
		g := 3
		switch {
		case m.signer && m.writable:
			g = 0
		case m.signer:
			g = 1
		case m.writable:
			g = 2
		}
		groups[g] = append(groups[g], k)
	}
	msg := Message{RecentBlockhash: blockhash}
	for _, g := range groups {
		msg.AccountKeys = append(msg.AccountKeys, g...)
	}
	if len(msg.AccountKeys) > 256 {
		return Message{}, fmt.Errorf("%w: too many accounts", ErrMessage)
	}
	msg.Header = Header{RequiredSignatures: uint8(len(groups[0]) + len(groups[1])), ReadonlySigned: uint8(len(groups[1])),
		ReadonlyUnsigned: uint8(len(groups[3]))}
	index := map[PublicKey]uint8{}
	for i, k := range msg.AccountKeys {
		index[k] = uint8(i)
	}
	for _, ix := range ixs {
		ci := CompiledInstruction{ProgramIndex: index[ix.Program], Data: append([]byte(nil), ix.Data...)}
		for _, a := range ix.Accounts {
			ci.Accounts = append(ci.Accounts, index[a.Key])
		}
		msg.Instructions = append(msg.Instructions, ci)
	}
	return msg, nil
}

func appendShortVec(b []byte, n int) []byte {
	for {
		c := byte(n & 0x7f)
		n >>= 7
		if n == 0 {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}

func readShortVec(b []byte) (int, int, error) {
	n, shift := 0, 0
	for i := 0; i < 3; i++ {
		if i >= len(b) {
			return 0, 0, ErrMessage
		}
		c := b[i]
		n |= int(c&0x7f) << shift
		if c&0x80 == 0 {
			if i > 0 && c == 0 {
				return 0, 0, fmt.Errorf("%w: non-canonical length", ErrMessage)
			}
			return n, i + 1, nil
		}
		shift += 7
	}
	return 0, 0, fmt.Errorf("%w: length too long", ErrMessage)
}

// Serialize returns the message in the wire format the signatures cover.
func (m Message) Serialize() []byte {
	out := []byte{m.Header.RequiredSignatures, m.Header.ReadonlySigned, m.Header.ReadonlyUnsigned}
	out = appendShortVec(out, len(m.AccountKeys))
	for _, k := range m.AccountKeys {
		out = append(out, k[:]...)
	}
	out = append(out, m.RecentBlockhash[:]...)
	out = appendShortVec(out, len(m.Instructions))
	for _, ix := range m.Instructions {
		out = append(out, ix.ProgramIndex)
		out = appendShortVec(out, len(ix.Accounts))
		out = append(out, ix.Accounts...)
		out = appendShortVec(out, len(ix.Data))
		out = append(out, ix.Data...)
	}
	return out
}

// ParseMessage decodes a legacy message and rejects trailing bytes and versioned messages.
func ParseMessage(b []byte) (Message, error) {
	var m Message
	if len(b) < 3 {
		return m, ErrMessage
	}
	if b[0]&0x80 != 0 {
		return m, fmt.Errorf("%w: versioned messages are not accepted", ErrMessage)
	}
	m.Header = Header{b[0], b[1], b[2]}
	p := 3
	n, k, err := readShortVec(b[p:])
	if err != nil {
		return m, err
	}
	p += k
	if len(b) < p+32*n+32 {
		return m, ErrMessage
	}
	for i := 0; i < n; i++ {
		m.AccountKeys = append(m.AccountKeys, PublicKey(b[p:p+32]))
		p += 32
	}
	copy(m.RecentBlockhash[:], b[p:p+32])
	p += 32
	nIx, k, err := readShortVec(b[p:])
	if err != nil {
		return m, err
	}
	p += k
	for i := 0; i < nIx; i++ {
		if p >= len(b) {
			return m, ErrMessage
		}
		ci := CompiledInstruction{ProgramIndex: b[p]}
		p++
		na, k, err := readShortVec(b[p:])
		if err != nil {
			return m, err
		}
		p += k
		if len(b) < p+na {
			return m, ErrMessage
		}
		ci.Accounts = append([]uint8(nil), b[p:p+na]...)
		p += na
		nd, k, err := readShortVec(b[p:])
		if err != nil {
			return m, err
		}
		p += k
		if len(b) < p+nd {
			return m, ErrMessage
		}
		ci.Data = append([]byte(nil), b[p:p+nd]...)
		p += nd
		if int(ci.ProgramIndex) >= n {
			return m, fmt.Errorf("%w: program index out of range", ErrMessage)
		}
		for _, a := range ci.Accounts {
			if int(a) >= n {
				return m, fmt.Errorf("%w: account index out of range", ErrMessage)
			}
		}
		m.Instructions = append(m.Instructions, ci)
	}
	if p != len(b) {
		return m, fmt.Errorf("%w: trailing bytes", ErrMessage)
	}
	if int(m.Header.RequiredSignatures) > n || m.Header.ReadonlySigned > m.Header.RequiredSignatures ||
		int(m.Header.ReadonlyUnsigned) > n-int(m.Header.RequiredSignatures) {
		return m, fmt.Errorf("%w: header out of range", ErrMessage)
	}
	return m, nil
}

// IsWritable reports whether the account at index i is writable.
func (m Message) IsWritable(i int) bool {
	req := int(m.Header.RequiredSignatures)
	if i < req {
		return i < req-int(m.Header.ReadonlySigned)
	}
	return i < len(m.AccountKeys)-int(m.Header.ReadonlyUnsigned)
}

// Transaction is a message plus one signature per required signer, in account-key order. A
// zero signature is an empty slot another party still has to fill.
type Transaction struct {
	Signatures [][64]byte
	Message    Message
}

// NewTransaction returns an unsigned transaction with an empty signature slot per required signer.
func NewTransaction(m Message) *Transaction {
	return &Transaction{Signatures: make([][64]byte, m.Header.RequiredSignatures), Message: m}
}

// Sign fills the slots of every given key that is a required signer.
func (t *Transaction) Sign(keys ...*Keypair) error {
	msg := t.Message.Serialize()
	for _, k := range keys {
		i := t.signerIndex(k.PublicKey())
		if i < 0 {
			return fmt.Errorf("solana: %s is not a signer of this message", k.PublicKey())
		}
		t.Signatures[i] = k.Sign(msg)
	}
	return nil
}

func (t *Transaction) signerIndex(k PublicKey) int {
	for i := 0; i < int(t.Message.Header.RequiredSignatures) && i < len(t.Message.AccountKeys); i++ {
		if t.Message.AccountKeys[i] == k {
			return i
		}
	}
	return -1
}

// VerifySignature checks the signature slot of k (false when k is not a signer or the slot is
// empty or wrong).
func (t *Transaction) VerifySignature(k PublicKey) bool {
	i := t.signerIndex(k)
	if i < 0 {
		return false
	}
	return Verify(k, t.Message.Serialize(), t.Signatures[i])
}

// ID is the transaction id: the first signature (the fee payer's) in base58.
func (t *Transaction) ID() string {
	if len(t.Signatures) == 0 {
		return ""
	}
	return base58.Encode(t.Signatures[0][:])
}

// Serialize returns the transaction in the wire format: signatures, then the message.
func (t *Transaction) Serialize() ([]byte, error) {
	out := appendShortVec(nil, len(t.Signatures))
	for _, s := range t.Signatures {
		out = append(out, s[:]...)
	}
	out = append(out, t.Message.Serialize()...)
	if len(out) > maxPacket {
		return nil, fmt.Errorf("solana: transaction is %d bytes, above the %d-byte limit", len(out), maxPacket)
	}
	return out, nil
}

// Base64 returns the serialized transaction encoded as standard base64.
func (t *Transaction) Base64() (string, error) {
	raw, err := t.Serialize()
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// ParseTransaction decodes a transaction in the wire format, or fails with ErrMessage.
func ParseTransaction(b []byte) (*Transaction, error) {
	if len(b) > maxPacket {
		return nil, fmt.Errorf("%w: too large", ErrMessage)
	}
	n, k, err := readShortVec(b)
	if err != nil {
		return nil, err
	}
	if len(b) < k+64*n {
		return nil, ErrMessage
	}
	t := &Transaction{}
	for i := 0; i < n; i++ {
		var s [64]byte
		copy(s[:], b[k+64*i:k+64*(i+1)])
		t.Signatures = append(t.Signatures, s)
	}
	if t.Message, err = ParseMessage(b[k+64*n:]); err != nil {
		return nil, err
	}
	if len(t.Signatures) != int(t.Message.Header.RequiredSignatures) {
		return nil, fmt.Errorf("%w: %d signatures for %d signers", ErrMessage, len(t.Signatures), t.Message.Header.RequiredSignatures)
	}
	return t, nil
}

// ParseTransactionBase64 decodes a base64 transaction, or fails with ErrMessage.
func ParseTransactionBase64(s string) (*Transaction, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: not base64", ErrMessage)
	}
	return ParseTransaction(b)
}

// ParseBlockhash decodes a base58 blockhash.
func ParseBlockhash(s string) ([32]byte, error) {
	b, err := base58.Decode(s)
	if err != nil || len(b) != 32 {
		return [32]byte{}, fmt.Errorf("solana: invalid blockhash %q", s)
	}
	return [32]byte(b), nil
}
