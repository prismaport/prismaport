package solana

import (
	"encoding/binary"

	"github.com/prismaport/prismaport/internal/solanaderive"
)

// Well-known program addresses used when building transactions.
var (
	SystemProgramID          = MustPublicKey("11111111111111111111111111111111")
	ComputeBudgetProgramID   = MustPublicKey("ComputeBudget111111111111111111111111111111")
	TokenProgramID           = solanaderive.TokenProgramID
	Token2022ProgramID       = MustPublicKey("TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb")
	AssociatedTokenProgramID = solanaderive.AssociatedTokenProgramID
)

// Account sizes in bytes of an SPL mint and an SPL token account.
const (
	MintSize         = 82
	TokenAccountSize = 165
)

// SetComputeUnitLimit caps the compute units the transaction may use.
func SetComputeUnitLimit(units uint32) Instruction {
	return Instruction{Program: ComputeBudgetProgramID, Data: binary.LittleEndian.AppendUint32([]byte{2}, units)}
}

// SetComputeUnitPrice sets the priority fee in micro-lamports per compute unit.
func SetComputeUnitPrice(microLamports uint64) Instruction {
	return Instruction{Program: ComputeBudgetProgramID, Data: binary.LittleEndian.AppendUint64([]byte{3}, microLamports)}
}

// SystemTransfer moves lamports from one account to another through the system program.
func SystemTransfer(from, to PublicKey, lamports uint64) Instruction {
	data := binary.LittleEndian.AppendUint32(nil, 2)
	return Instruction{Program: SystemProgramID, Accounts: []AccountMeta{WS(from), W(to)},
		Data: binary.LittleEndian.AppendUint64(data, lamports)}
}

// AssociatedTokenAddress is the classic SPL Token ATA of (owner, mint).
func AssociatedTokenAddress(owner, mint PublicKey) (PublicKey, error) {
	return solanaderive.AssociatedTokenAddress(owner, mint)
}

// CreateATAIdempotent creates the ATA of (owner, mint) if it does not exist; payer funds the rent.
func CreateATAIdempotent(payer, ata, owner, mint, tokenProgram PublicKey) Instruction {
	return Instruction{Program: AssociatedTokenProgramID, Accounts: []AccountMeta{WS(payer), W(ata), R(owner), R(mint),
		R(SystemProgramID), R(tokenProgram)}, Data: []byte{1}}
}

// TransferChecked moves amount base units; extra read-only accounts (Solana Pay reference keys)
// are ignored by the token program when the authority is a plain wallet.
func TransferChecked(tokenProgram, source, mint, dest, authority PublicKey, amount uint64, decimals uint8, extra ...PublicKey) Instruction {
	data := binary.LittleEndian.AppendUint64([]byte{12}, amount)
	data = append(data, decimals)
	accs := []AccountMeta{W(source), R(mint), W(dest), {Key: authority, Signer: true}}
	for _, e := range extra {
		accs = append(accs, R(e))
	}
	return Instruction{Program: tokenProgram, Accounts: accs, Data: data}
}

// TokenAmount reads the amount of a classic SPL token account (bytes 64..72 of its data).
func TokenAmount(data []byte) (amount uint64, mint, owner PublicKey, ok bool) {
	if len(data) < TokenAccountSize {
		return 0, mint, owner, false
	}
	copy(mint[:], data[0:32])
	copy(owner[:], data[32:64])
	return binary.LittleEndian.Uint64(data[64:72]), mint, owner, true
}

// TokenAccountUsable reports whether an account, given its owner program and data, is an
// initialized and unfrozen classic SPL Token account of mint (state byte at offset 108).
func TokenAccountUsable(ownerProgram string, data []byte, mint PublicKey) bool {
	if ownerProgram != TokenProgramID.String() || len(data) != TokenAccountSize {
		return false
	}
	return PublicKey(data[0:32]) == mint && data[108] == 1
}

// TokenAccountFrozen reports whether an account is a classic SPL Token account of mint that its
// mint's freeze authority froze (state byte 2): no transfer into it succeeds until it is thawed.
func TokenAccountFrozen(ownerProgram string, data []byte, mint PublicKey) bool {
	if ownerProgram != TokenProgramID.String() || len(data) != TokenAccountSize {
		return false
	}
	return PublicKey(data[0:32]) == mint && data[108] == 2
}
