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
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/types/bal"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
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
		if len(al.Accounts) == 0 {
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

// newOsakaPathChain mirrors newOsakaChain on the path scheme with archive-mode
// indexing and a real ancient store, so state histories, the history index and
// SetHead's reverse-diff rollback all behave like a production datadir.
func newOsakaPathChain(t *testing.T, disk ethdb.Database, alloc types.GenesisAlloc, timestamp uint64) *BlockChain {
	t.Helper()
	cfg := *params.MergedTestChainConfig // Osaka, no Amsterdam
	cfg.WithBAL = true
	opts := DefaultConfig().WithStateScheme(rawdb.PathScheme).WithArchive(true)
	opts.PrefetchWorkers = runtime.NumCPU()
	opts.TrieJournalDirectory = t.TempDir()
	bc, err := NewBlockChain(disk, &Genesis{Config: &cfg, Alloc: alloc, Timestamp: timestamp}, beacon.New(ethash.NewFaker()), opts)
	if err != nil {
		t.Fatalf("create Osaka path chain: %v", err)
	}
	t.Cleanup(bc.Stop)
	return bc
}

// TestBALReplayRoundTripAfterRewind mirrors the mainnet replay bed end to end:
// a path-scheme archive chain grows past the diff-layer allowance so early
// parents are reachable only through state history, BALs are recomputed against
// both live and historic parent state (which must agree), the head is rewound
// below the disk layer via reverse diffs, and the blocks are re-imported through
// the BAL consume path, which validates every state root.
func TestBALReplayRoundTripAfterRewind(t *testing.T) {
	const (
		n      = 48
		rewind = 8
	)
	env := newBALTestEnv(nil)
	engine := beacon.New(ethash.NewFaker())

	// A recent genesis keeps the history-index sync gate open, matching the
	// widened staleness window on the replay bed.
	ts := uint64(time.Now().Add(-time.Hour).Unix())

	// A storage scratchpad: 64-byte calldata does sstore(key=calldata[0:32],
	// value=calldata[32:64]); 32-byte calldata does sload(key=calldata[0:32]).
	runtime := []byte{
		0x36, 0x60, 0x20, 0x14, 0x60, 0x0f, 0x57, // CALLDATASIZE == 32 → jump to sload
		0x60, 0x20, 0x35, 0x60, 0x00, 0x35, 0x55, 0x00, // sstore path
		0x5b, 0x60, 0x00, 0x35, 0x54, 0x50, 0x00, // sload path
	}
	initcode := append(append([]byte{0x75}, runtime...), 0x60, 0x00, 0x52, 0x60, 0x16, 0x60, 0x0a, 0xf3)
	contract := crypto.CreateAddress(env.from, 0)
	store := func(key, val byte) []byte {
		data := make([]byte, 64)
		data[31], data[63] = key, val
		return data
	}
	load := func(key byte) []byte {
		data := make([]byte, 32)
		data[31] = key
		return data
	}
	var nonce uint64
	tx := func(to *common.Address, value int64, data []byte) *types.Transaction {
		// Non-zero tip so the coinbase collects a balance change per tx,
		// like a mainnet fee recipient does.
		signed := env.tx(nonce, to, big.NewInt(value), 500_000, 1, data)
		nonce++
		return signed
	}

	genCfg := *params.MergedTestChainConfig
	_, blocks, _ := GenerateChainWithGenesis(&Genesis{Config: &genCfg, Alloc: env.gspec.Alloc, Timestamp: ts}, engine, n, func(i int, b *BlockGen) {
		b.SetParentBeaconRoot(common.Hash{}) // process EIP-4788 so re-execution matches
		if i == 0 {
			b.AddTx(tx(nil, 0, initcode))
			return
		}
		// Fresh-account transfer, storage churn (new slots, overwrites and
		// zeroings via the 7-slot cycle), and existing-account payments.
		to := common.BigToAddress(big.NewInt(int64(0xdead0000 + i)))
		b.AddTx(tx(&to, 1000, nil))
		b.AddTx(tx(&contract, 0, store(byte(i%7+1), byte(i))))
		if i%5 == 0 {
			b.AddTx(tx(&contract, 0, store(byte(i%7+1), 0))) // zero it back out
			// Read the just-zeroed slot from a later tx in the same block: the
			// sequential pipeline serves this from pending storage while the
			// parallel one serves it from the access list, and the emptiness
			// signal must come out identical either way.
			b.AddTx(tx(&contract, 0, load(byte(i%7+1))))
		}
		if i%9 == 0 {
			b.AddTx(tx(&env.from, 7, nil)) // self transfer touches only existing state
		}
		if i == 20 {
			b.AddTx(tx(nil, 0, initcode)) // a deployment inside the replayed range
		}
		// Withdrawals: one to a fresh account, one to the tx sender (a
		// block-level credit on an account transactions also touched), plus a
		// second credit to the fresh account every fourth block (accumulation)
		// and a zero-amount one (access recorded without a balance change).
		fresh := common.BigToAddress(big.NewInt(int64(0xaa00 + i)))
		b.AddWithdrawal(&types.Withdrawal{Address: fresh, Amount: 100})
		b.AddWithdrawal(&types.Withdrawal{Address: env.from, Amount: 50})
		if i%4 == 0 {
			b.AddWithdrawal(&types.Withdrawal{Address: fresh, Amount: 25})
		}
		if i%6 == 0 {
			b.AddWithdrawal(&types.Withdrawal{Address: common.BigToAddress(big.NewInt(int64(0xbb00 + i))), Amount: 0})
		}
	})

	disk, err := rawdb.Open(rawdb.NewMemoryDatabase(), rawdb.OpenOptions{Ancient: t.TempDir()})
	if err != nil {
		t.Fatalf("open disk db: %v", err)
	}
	builder := newOsakaPathChain(t, disk, env.gspec.Alloc, ts)
	als := make([]*bal.BlockAccessList, n)
	for i, b := range blocks {
		if _, err := builder.InsertChain([]*types.Block{b}); err != nil {
			t.Fatalf("insert #%d: %v", b.NumberU64(), err)
		}
		al, err := builder.RecomputeAccessList(b) // parent state is live here
		if err != nil {
			t.Fatalf("live recompute #%d: %v", b.NumberU64(), err)
		}
		als[i] = al
	}

	// Flatten the trie down to head so every state lands in the freezer as a
	// reverse diff and the disk layer sits at the head — the shape of a synced
	// production datadir, where every parent is served through state history.
	if err := builder.triedb.Commit(blocks[n-1].Root(), false); err != nil {
		t.Fatalf("commit trie to head: %v", err)
	}

	// Reopen on the built datadir, the way the export process does on the
	// replay bed, and wait for the history index to back-fill and serve.
	builder.Stop()
	bc := newOsakaPathChain(t, disk, env.gspec.Alloc, ts)
	genesisRoot := bc.GetHeaderByNumber(0).Root
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if _, err := bc.triedb.HistoricStateReader(genesisRoot); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("history index never became servable: %v", err)
		}
	}

	// Every parent now lives only in state history. The recomputed BAL must
	// not depend on which path served the parent state.
	for i, b := range blocks {
		al, err := bc.RecomputeAccessList(b)
		if err != nil {
			root := bc.GetHeaderByNumber(b.NumberU64() - 1).Root
			if _, e := bc.triedb.HistoricStateReader(root); e != nil {
				t.Logf("historic state reader for #%d: %v", b.NumberU64()-1, e)
			}
			if _, e := bc.triedb.HistoricNodeReader(root); e != nil {
				t.Logf("historic node reader for #%d: %v", b.NumberU64()-1, e)
			}
			t.Fatalf("historic recompute #%d: %v", b.NumberU64(), err)
		}
		if got, want := al.Hash(), als[i].Hash(); got != want {
			t.Fatalf("BAL for #%d differs between live and historic parent state: live %x historic %x", b.NumberU64(), want, got)
		}
	}

	// Rewind below the disk layer (reverse-diff rollback), then re-import the
	// blocks with BALs attached, through the consume path.
	if err := bc.SetHead(rewind); err != nil {
		t.Fatalf("set head: %v", err)
	}
	if got := bc.CurrentBlock().Number.Uint64(); got != rewind {
		t.Fatalf("rewound head = %d, want %d", got, rewind)
	}
	for i := rewind; i < n; i++ {
		if _, err := bc.InsertChain([]*types.Block{blocks[i].WithAccessListUnsafe(als[i])}); err != nil {
			for _, acct := range als[i].Accounts {
				t.Logf("BAL entry %x: %d balance, %d nonce, %d storage changes", acct.Address, len(acct.BalanceChanges), len(acct.NonceChanges), len(acct.StorageChanges))
			}
			t.Logf("empty accounts: %v", als[i].EmptyAccounts)
			t.Logf("empty slots: %v", als[i].EmptySlots)
			t.Fatalf("consume after rewind failed at #%d (%d txs, %d withdrawals): %v",
				blocks[i].NumberU64(), len(blocks[i].Transactions()), len(blocks[i].Withdrawals()), err)
		}
	}
	if got := bc.CurrentBlock().Number.Uint64(); got != n {
		t.Fatalf("head after replay = %d, want %d", got, n)
	}
}
