package solanaderive

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// vectorCopy is a shared vector file and the copy solana/ keeps of it.
type vectorCopy struct{ canonical, copy, fix string }

// solana/ is published on its own (the prismaport-contracts-solana-v1 repo), so the program tests
// read their own copies of the shared vectors. The canonical files stay where the Go and Solidity
// tests read them; this test keeps the copies byte for byte identical.
func TestSolanaVectorCopiesMatchCanonical(t *testing.T) {
	checkVectorCopies(t, []vectorCopy{
		// Written by contracts/script/GenerateVectors.s.sol.
		{
			"contracts/test/vectors/splits.json", "solana/vectors/splits.json",
			"cp contracts/test/vectors/splits.json solana/vectors/splits.json",
		},
	})
}

func checkVectorCopies(t *testing.T, pairs []vectorCopy) {
	t.Helper()
	root := filepath.Join("..", "..")
	for _, p := range pairs {
		want, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p.canonical)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p.copy)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from %s; after regenerating, run: %s", p.copy, p.canonical, p.fix)
		}
	}
}
