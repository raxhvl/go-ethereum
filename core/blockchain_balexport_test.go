// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"math/big"
	"runtime"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/types/bal"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// generateBALBlock builds a single Amsterdam block carrying a value transfer so it
// has a non-trivial block access list, and returns the block.
func generateBALBlock(t *testing.T) *types.Block {
	t.Helper()
	env := newBALTestEnv(nil)
	engine := beacon.New(ethash.NewFaker())
	_, blocks, _ := GenerateChainWithGenesis(env.gspec, engine, 1, func(_ int, b *BlockGen) {
		to := common.BigToAddress(big.NewInt(0xc0ffee))
		b.AddTx(env.tx(0, &to, big.NewInt(1000), txGasNewAccount, 0, nil))
	})
	if blocks[0].AccessList() == nil {
		t.Fatal("generated block missing access list — Amsterdam not active?")
	}
	return blocks[0]
}

// TestBALExportEntrySerializationRoundtrip proves the `--with-bal` export format is
// lossless: a block paired with its RLP-encoded access list sidecar survives the
// BALExportEntry encode→decode cycle that export writes and import reads, and the
// decoded sidecar re-attaches to an identical access list. This isolates the new
// serialization plumbing (phase 2) from block re-execution.
func TestBALExportEntrySerializationRoundtrip(t *testing.T) {
	src := generateBALBlock(t)
	wantBlockHash := src.Hash()
	wantBALHash := src.AccessList().Hash()

	// Mirror writeBALEntry: RLP the sidecar, then RLP the {block, sidecar} entry.
	balRLP, err := rlp.EncodeToBytes(src.AccessList())
	if err != nil {
		t.Fatalf("encode BAL sidecar: %v", err)
	}
	enc, err := rlp.EncodeToBytes(&BALExportEntry{Block: src, BAL: balRLP})
	if err != nil {
		t.Fatalf("encode export entry: %v", err)
	}

	// Mirror ImportChainWithBAL: decode the entry, decode the sidecar, re-attach.
	var got BALExportEntry
	if err := rlp.DecodeBytes(enc, &got); err != nil {
		t.Fatalf("decode export entry: %v", err)
	}
	if got.Block.Hash() != wantBlockHash {
		t.Fatalf("block hash changed across roundtrip: got %x want %x", got.Block.Hash(), wantBlockHash)
	}
	decBAL := new(bal.BlockAccessList)
	if err := rlp.DecodeBytes(got.BAL, decBAL); err != nil {
		t.Fatalf("decode BAL sidecar: %v", err)
	}
	if decBAL.Hash() != wantBALHash {
		t.Fatalf("BAL hash changed across roundtrip: got %x want %x", decBAL.Hash(), wantBALHash)
	}
	if h := got.Block.WithAccessListUnsafe(decBAL).AccessList().Hash(); h != wantBALHash {
		t.Fatalf("re-attached access list hash mismatch: got %x want %x", h, wantBALHash)
	}
}

// newOsakaChain builds a BlockChain on the (non-Amsterdam) Osaka test config, with
// WithBAL optionally forced on plus a non-zero PrefetchWorkers (the consume path
// requires it).
func newOsakaChain(t *testing.T, alloc types.GenesisAlloc, withBAL bool) *BlockChain {
	t.Helper()
	cfg := *params.MergedTestChainConfig // Osaka, no Amsterdam
	cfg.WithBAL = withBAL
	opts := DefaultConfig().WithStateScheme(rawdb.HashScheme)
	opts.PrefetchWorkers = runtime.NumCPU()
	bc, err := NewBlockChain(rawdb.NewMemoryDatabase(), &Genesis{Config: &cfg, Alloc: alloc}, beacon.New(ethash.NewFaker()), opts)
	if err != nil {
		t.Fatalf("create Osaka chain (withBAL=%v): %v", withBAL, err)
	}
	t.Cleanup(bc.Stop)
	return bc
}

// TestWithBALConstructConsumeVerify exercises the full machinery on a pre-Amsterdam
// (Osaka) fork with WithBAL: construct a BAL per block, attach it, run the consume
// path, and confirm the provided-vs-computed verification — both that a correct BAL
// is accepted and that a corrupted (wrong) one is rejected.
func TestWithBALConstructConsumeVerify(t *testing.T) {
	const n = 3
	env := newBALTestEnv(nil) // reused only for its key/signer/alloc
	engine := beacon.New(ethash.NewFaker())

	// Generate clean Osaka blocks (no WithBAL → GenerateChain attaches no BAL).
	genCfg := *params.MergedTestChainConfig
	_, blocks, _ := GenerateChainWithGenesis(&Genesis{Config: &genCfg, Alloc: env.gspec.Alloc}, engine, n, func(i int, b *BlockGen) {
		b.SetParentBeaconRoot(common.Hash{}) // process EIP-4788 so re-execution matches
		to := common.BigToAddress(big.NewInt(int64(0xc0ffee + i)))
		b.AddTx(env.tx(uint64(i), &to, big.NewInt(1000), txGasNewAccount, 0, nil))
	})
	if blocks[0].AccessList() != nil {
		t.Fatal("source blocks should carry no BAL (generated without WithBAL)")
	}

	// Baseline: the generated blocks must insert into a plain (no-WithBAL) chain.
	if _, err := newOsakaChain(t, env.gspec.Alloc, false).InsertChain(blocks); err != nil {
		t.Fatalf("baseline insert (no WithBAL) failed — test setup issue: %v", err)
	}

	// CONSTRUCT: insert into a WithBAL chain (must match the baseline roots, i.e.
	// execution is unchanged), then recompute each block's BAL.
	src := newOsakaChain(t, env.gspec.Alloc, true)
	if _, err := src.InsertChain(blocks); err != nil {
		t.Fatalf("WithBAL changed execution (blocks no longer validate): %v", err)
	}
	bals := make([]*bal.BlockAccessList, n)
	for i, b := range blocks {
		al, err := src.RecomputeAccessList(b)
		if err != nil {
			t.Fatalf("recompute BAL for #%d: %v", b.NumberU64(), err)
		}
		if len(*al) == 0 {
			t.Fatalf("recomputed BAL for #%d is empty — WithBAL not constructing", b.NumberU64())
		}
		bals[i] = al
	}

	// CONSUME + VERIFY (happy path): attach correct BALs, import through the consume
	// path. Success means each block's reconstructed BAL matched the attached one.
	dst := newOsakaChain(t, env.gspec.Alloc, true)
	attached := make([]*types.Block, n)
	for i, b := range blocks {
		attached[i] = b.WithAccessListUnsafe(bals[i])
	}
	if _, err := dst.InsertChain(attached); err != nil {
		t.Fatalf("consume+verify of correct BALs failed: %v", err)
	}
	if got := dst.CurrentBlock().Number.Uint64(); got != n {
		t.Fatalf("import head = %d, want %d", got, n)
	}

	// VERIFY (corruption path): a wrong BAL must be rejected. Block 1 touches state,
	// so an empty access list cannot match what execution reconstructs.
	bad := newOsakaChain(t, env.gspec.Alloc, true)
	corrupt := blocks[0].WithAccessListUnsafe(new(bal.BlockAccessList))
	if _, err := bad.InsertChain([]*types.Block{corrupt}); err == nil {
		t.Fatal("consume path accepted a corrupted (empty) BAL — verification not enforced")
	}
}
