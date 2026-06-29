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
	"context"
	"fmt"
	"io"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/types/bal"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

// BALExportEntry is one record in a `geth export --with-bal` stream: a canonical
// block paired with the RLP-encoded EIP-7928 block access list recomputed for it.
// The pair is itself RLP-encoded, so the export is a self-framing sequence of
// these entries that the matching import mode reads back in order.
type BALExportEntry struct {
	Block *types.Block
	BAL   []byte // RLP-encoded bal.BlockAccessList sidecar
}

// RecomputeAccessList re-executes block against its parent state and returns the
// freshly built EIP-7928 block access list (in encoding form). It validates and
// persists nothing — it only orchestrates the existing processor. Amsterdam must
// be active in the chain config or no BAL is produced.
func (bc *BlockChain) RecomputeAccessList(block *types.Block) (*bal.BlockAccessList, error) {
	parent := bc.GetHeaderByHash(block.ParentHash())
	if parent == nil {
		return nil, fmt.Errorf("parent header %x not found for block #%d", block.ParentHash(), block.NumberU64())
	}
	statedb, err := bc.StateAt(parent)
	if err != nil {
		return nil, fmt.Errorf("state at parent #%d (%x): %w", parent.Number.Uint64(), parent.Root, err)
	}
	res, err := bc.processor.Process(context.Background(), block, statedb, bc.jumpDestCache, bc.cfg.VmConfig)
	if err != nil {
		return nil, err
	}
	if res.Bal == nil {
		return nil, fmt.Errorf("no block access list produced for #%d (is Amsterdam active?)", block.NumberU64())
	}
	return res.Bal.ToEncodingObj(), nil
}

// ExportNWithBAL writes blocks [first,last] to w in the `--with-bal` format: a
// stream of BALExportEntry records, each pairing a block with its recomputed BAL.
func (bc *BlockChain) ExportNWithBAL(w io.Writer, first, last uint64) error {
	return bc.exportN(w, first, last, bc.writeBALEntry)
}

// writeBALEntry recomputes the block's access list and writes it paired with the
// block as a BALExportEntry. Genesis has no parent state, so it carries an empty
// list. When the header already commits to a BAL (an Amsterdam-native chain) the
// recomputed hash is checked against it — a mismatch means our recompute diverged
// from the builder, which is logged per block.
func (bc *BlockChain) writeBALEntry(w io.Writer, block *types.Block) error {
	al := new(bal.BlockAccessList)
	if block.NumberU64() != 0 {
		var err error
		if al, err = bc.RecomputeAccessList(block); err != nil {
			return fmt.Errorf("recompute BAL for #%d: %w", block.NumberU64(), err)
		}
	}
	balRLP, err := rlp.EncodeToBytes(al)
	if err != nil {
		return fmt.Errorf("encode BAL for #%d: %w", block.NumberU64(), err)
	}
	if h := block.Header().BlockAccessListHash; h != nil {
		if got := crypto.Keccak256Hash(balRLP); got != *h {
			log.Warn("Recomputed BAL hash mismatch", "block", block.NumberU64(), "header", h.Hex(), "recomputed", got.Hex())
		}
	}
	return rlp.Encode(w, &BALExportEntry{Block: block, BAL: balRLP})
}
