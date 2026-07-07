// Copyright 2025 The go-ethereum Authors
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

package bal

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"
)

//go:generate go run github.com/ethereum/go-ethereum/rlp/rlpgen -out bal_encoding_rlp_generated.go -type AccountAccess -decoder

// These are objects used as input for the access list encoding. They mirror
// the spec format.

// BlockAccessList is the encoding format of ConstructionBlockAccessList. On top
// of the EIP-7928 account list it carries the block-start emptiness signal:
// which accessed accounts were non-existent, and which accessed slots were zero,
// at the block-start pre-state. Wire form:
//
//	[ accounts, accountBitmap, slotBitmap ]
//
// One bit per item in canonical order — accounts in list order; slots per
// account, storage reads then storage changes. Bits are packed LSB-first
// (bit i -> byte i>>3, position i&7); the order is consensus-critical.
type BlockAccessList struct {
	Accounts      []AccountAccess
	EmptyAccounts map[common.Address]struct{}
	EmptySlots    map[common.Address]map[common.Hash]struct{}
}

func bitmapLen(n int) int          { return (n + 7) >> 3 }
func setBit(bm []byte, i int)      { bm[i>>3] |= 1 << uint(i&7) }
func getBit(bm []byte, i int) bool { return bm[i>>3]&(1<<uint(i&7)) != 0 }

// slotCount returns the number of items in the global slot order.
func (e *BlockAccessList) slotCount() int {
	n := 0
	for i := range e.Accounts {
		n += len(e.Accounts[i].StorageReads) + len(e.Accounts[i].StorageChanges)
	}
	return n
}

// walkSlots visits each item in the global slot order, passing its bit position,
// account, and slot key.
func (e *BlockAccessList) walkSlots(fn func(pos int, addr common.Address, slot common.Hash)) {
	pos := 0
	for i := range e.Accounts {
		a := &e.Accounts[i]
		for _, r := range a.StorageReads {
			fn(pos, a.Address, common.Hash(r.Bytes32()))
			pos++
		}
		for _, c := range a.StorageChanges {
			fn(pos, a.Address, common.Hash(c.Slot.Bytes32()))
			pos++
		}
	}
}

// EncodeRLP implements rlp.Encoder. Emptiness is projected onto the two bitmaps
// by walking the account list, so only emptiness of listed items is encoded.
func (e BlockAccessList) EncodeRLP(w io.Writer) error {
	buf := rlp.NewEncoderBuffer(w)
	outer := buf.List()
	accounts := buf.List()
	for i := range e.Accounts {
		if err := e.Accounts[i].EncodeRLP(buf); err != nil {
			return err
		}
	}
	buf.ListEnd(accounts)

	acctBM := make([]byte, bitmapLen(len(e.Accounts)))
	for i := range e.Accounts {
		if _, ok := e.EmptyAccounts[e.Accounts[i].Address]; ok {
			setBit(acctBM, i)
		}
	}
	slotBM := make([]byte, bitmapLen(e.slotCount()))
	e.walkSlots(func(pos int, addr common.Address, slot common.Hash) {
		if _, ok := e.EmptySlots[addr][slot]; ok {
			setBit(slotBM, pos)
		}
	})
	buf.WriteBytes(acctBM)
	buf.WriteBytes(slotBM)
	buf.ListEnd(outer)
	return buf.Flush()
}

// DecodeRLP implements rlp.Decoder. The emptiness maps are rebuilt from the
// bitmaps; a size mismatch means the bitmaps don't describe the account list.
func (e *BlockAccessList) DecodeRLP(s *rlp.Stream) error {
	var dec BlockAccessList
	if _, err := s.List(); err != nil { // outer
		return err
	}
	if _, err := s.List(); err != nil { // accounts
		return err
	}
	for s.MoreDataInList() {
		var a AccountAccess
		if err := a.DecodeRLP(s); err != nil {
			return err
		}
		dec.Accounts = append(dec.Accounts, a)
	}
	if err := s.ListEnd(); err != nil {
		return err
	}
	acctBM, err := s.Bytes()
	if err != nil {
		return err
	}
	slotBM, err := s.Bytes()
	if err != nil {
		return err
	}
	if err := s.ListEnd(); err != nil { // outer
		return err
	}

	if want := bitmapLen(len(dec.Accounts)); len(acctBM) != want {
		return fmt.Errorf("account emptiness bitmap size mismatch: got %d bytes, want %d for %d accounts", len(acctBM), want, len(dec.Accounts))
	}
	if want := bitmapLen(dec.slotCount()); len(slotBM) != want {
		return fmt.Errorf("slot emptiness bitmap size mismatch: got %d bytes, want %d for %d slots", len(slotBM), want, dec.slotCount())
	}
	for i := range dec.Accounts {
		if getBit(acctBM, i) {
			if dec.EmptyAccounts == nil {
				dec.EmptyAccounts = make(map[common.Address]struct{})
			}
			dec.EmptyAccounts[dec.Accounts[i].Address] = struct{}{}
		}
	}
	dec.walkSlots(func(pos int, addr common.Address, slot common.Hash) {
		if !getBit(slotBM, pos) {
			return
		}
		if dec.EmptySlots == nil {
			dec.EmptySlots = make(map[common.Address]map[common.Hash]struct{})
		}
		if dec.EmptySlots[addr] == nil {
			dec.EmptySlots[addr] = make(map[common.Hash]struct{})
		}
		dec.EmptySlots[addr][slot] = struct{}{}
	})
	*e = dec
	return nil
}

// Validate returns an error if the contents of the access list are not ordered
// according to the spec or any code changes are contained which exceed protocol
// max code size.
func (e *BlockAccessList) Validate(blockGasLimit uint64, blockTxCount int) error {
	if !slices.IsSortedFunc(e.Accounts, func(a, b AccountAccess) int {
		return bytes.Compare(a.Address[:], b.Address[:])
	}) {
		return errors.New("block access list accounts not in lexicographic order")
	}
	for _, entry := range e.Accounts {
		if err := entry.validate(blockTxCount + 1); err != nil {
			return err
		}
	}
	return e.ValidateSize(blockGasLimit)
}

// itemCount returns the number of items in the BAL for EIP-7928 size-constraint
// purposes: the count of distinct addresses plus every storage key (writes +
// reads) carried by those accounts. A storage slot is counted once regardless
// of how many transactions wrote to it.
func (e *BlockAccessList) itemCount() uint64 {
	count := uint64(len(e.Accounts)) // distinct addresses
	for i := range e.Accounts {
		count += uint64(len(e.Accounts[i].StorageChanges)) + uint64(len(e.Accounts[i].StorageReads))
	}
	return count
}

// ValidateSize returns an error if the BAL violates the EIP-7928 size
// constraint for the given block gas limit:
//
//	itemCount() <= blockGasLimit / params.BALItemCost
func (e *BlockAccessList) ValidateSize(blockGasLimit uint64) error {
	items := e.itemCount()
	limit := blockGasLimit / params.BALItemCost
	if items > limit {
		return fmt.Errorf("block access list exceeds size constraint: items=%d, limit=%d (block gas limit %d / %d)",
			items, limit, blockGasLimit, params.BALItemCost)
	}
	return nil
}

// Hash computes the keccak256 hash of the access list
func (e *BlockAccessList) Hash() common.Hash {
	var enc bytes.Buffer
	if err := e.EncodeRLP(&enc); err != nil {
		// Errors here are related to BAL values exceeding maximum size defined
		// by the spec. Return empty hash because these cases are not expected
		// to be hit under reasonable conditions.
		return common.Hash{}
	}
	return crypto.Keccak256Hash(enc.Bytes())
}

// EIP-7928 encoding types. Field names and JSON keys mirror the
// execution-spec-tests Pydantic models in
// `src/ethereum_test_types/block_access_list/account_changes.py`. Hex
// formatting on JSON output is supplied via the gencodec overrides
// below.

//go:generate go run github.com/fjl/gencodec -type encodingStorageWrite -field-override encodingStorageWriteMarshaling -out gen_encoding_storage_write_json.go
//go:generate go run github.com/fjl/gencodec -type encodingSlotChanges -field-override encodingSlotChangesMarshaling -out gen_encoding_slot_changes_json.go
//go:generate go run github.com/fjl/gencodec -type encodingBalanceChange -field-override encodingBalanceChangeMarshaling -out gen_encoding_balance_change_json.go
//go:generate go run github.com/fjl/gencodec -type encodingAccountNonce -field-override encodingAccountNonceMarshaling -out gen_encoding_account_nonce_json.go
//go:generate go run github.com/fjl/gencodec -type encodingCodeChange -field-override encodingCodeChangeMarshaling -out gen_encoding_code_change_json.go
//go:generate go run github.com/fjl/gencodec -type AccountAccess -field-override accountAccessMarshaling -out gen_account_access_json.go

// encodingStorageWrite is one transaction's write to a storage slot.
type encodingStorageWrite struct {
	BlockAccessIndex uint32       `json:"blockAccessIndex"`
	PostValue        *uint256.Int `json:"postValue"`
}

type encodingStorageWriteMarshaling struct {
	BlockAccessIndex hexutil.Uint64
	PostValue        *hexutil.U256
}

// encodingSlotChanges aggregates all per-tx writes to a single storage slot.
type encodingSlotChanges struct {
	Slot        *uint256.Int           `json:"slot"`
	SlotChanges []encodingStorageWrite `json:"slotChanges"`
}

type encodingSlotChangesMarshaling struct {
	Slot *hexutil.U256
}

func isStrictlySortedFunc[S ~[]E, E any](x S, cmp func(a, b E) int) bool {
	for i := 1; i < len(x); i++ {
		if cmp(x[i-1], x[i]) >= 0 {
			return false // includes both unsorted and duplicate
		}
	}
	return true
}

// validate asserts that the encodingSlotWrites contain storage modfications
// which are ordered ascending by transaction index and contain no duplicate
// modifications for a given index.
func (e *encodingSlotChanges) validate(maxBALIndex int) error {
	// Each SlotChanges entry MUST contain at least one StorageChange.
	if len(e.SlotChanges) == 0 {
		return errors.New("empty slot changes")
	}
	// Each storage key MUST appear at most once in storage_changes per account.
	if !isStrictlySortedFunc(e.SlotChanges, func(a, b encodingStorageWrite) int {
		return cmp.Compare(a.BlockAccessIndex, b.BlockAccessIndex)
	}) {
		return errors.New("storage write indexes must be unique and sorted")
	}
	if len(e.SlotChanges) > 0 && int(e.SlotChanges[len(e.SlotChanges)-1].BlockAccessIndex) > maxBALIndex {
		return fmt.Errorf("storage write index exceeds limit, index: %d, limit: %d", e.SlotChanges[len(e.SlotChanges)-1].BlockAccessIndex, maxBALIndex)
	}
	return nil
}

// encodingBalanceChange is one transaction's post-state balance for an account.
type encodingBalanceChange struct {
	BlockAccessIndex uint32       `json:"blockAccessIndex"`
	PostBalance      *uint256.Int `json:"postBalance"`
}

type encodingBalanceChangeMarshaling struct {
	BlockAccessIndex hexutil.Uint64
	PostBalance      *hexutil.U256
}

// encodingAccountNonce is one transaction's post-state nonce for an account.
type encodingAccountNonce struct {
	BlockAccessIndex uint32 `json:"blockAccessIndex"`
	PostNonce        uint64 `json:"postNonce"`
}

type encodingAccountNonceMarshaling struct {
	BlockAccessIndex hexutil.Uint64
	PostNonce        hexutil.Uint64
}

// encodingCodeChange is one transaction's deployed runtime bytecode for an account.
type encodingCodeChange struct {
	BlockAccessIndex uint32 `json:"blockAccessIndex"`
	NewCode          []byte `json:"newCode"`
}

type encodingCodeChangeMarshaling struct {
	BlockAccessIndex hexutil.Uint64
	NewCode          hexutil.Bytes
}

// AccountAccess is the encoding format of ConstructionAccountAccess.
type AccountAccess struct {
	Address        common.Address          `json:"address"`
	StorageChanges []encodingSlotChanges   `json:"storageChanges"`
	StorageReads   []*uint256.Int          `json:"storageReads"`
	BalanceChanges []encodingBalanceChange `json:"balanceChanges"`
	NonceChanges   []encodingAccountNonce  `json:"nonceChanges"`
	CodeChanges    []encodingCodeChange    `json:"codeChanges"`
}

type accountAccessMarshaling struct {
	StorageReads []*hexutil.U256
}

// validate converts the account accesses out of encoding format.
// If any of the keys in the encoding object are not ordered according to the
// spec, an error is returned.
func (e *AccountAccess) validate(maxBALIndex int) error {
	// Check the storage writes are sorted in order, and unique by slot
	if !isStrictlySortedFunc(e.StorageChanges, func(a, b encodingSlotChanges) int {
		return a.Slot.Cmp(b.Slot)
	}) {
		return errors.New("storage write slots must be unique and sorted")
	}
	// Check the validity of each storage slot's mutations
	for _, slotWrites := range e.StorageChanges {
		if err := slotWrites.validate(maxBALIndex); err != nil {
			return err
		}
	}

	// Check the storage read slots are sorted in order, and unique by slot
	if !isStrictlySortedFunc(e.StorageReads, func(a, b *uint256.Int) int {
		return a.Cmp(b)
	}) {
		return errors.New("storage read slots must be unique and sorted")
	}

	// Check that the set of written storage slots does not intersect with the
	// set of read slots.
	var (
		readKeys  = make(map[common.Hash]struct{}, len(e.StorageReads))
		writeKeys = make(map[common.Hash]struct{}, len(e.StorageChanges))
	)
	for _, rk := range e.StorageReads {
		readKey := common.BytesToHash(rk.Bytes())
		readKeys[readKey] = struct{}{}
	}
	for _, write := range e.StorageChanges {
		writeKey := common.BytesToHash(write.Slot.Bytes())
		writeKeys[writeKey] = struct{}{}
	}
	for readKey := range readKeys {
		if _, ok := writeKeys[readKey]; ok {
			return errors.New("storage key reported in both read/write sets")
		}
	}

	// Check the balance changes are sorted in order, and unique by tx index
	if !isStrictlySortedFunc(e.BalanceChanges, func(a, b encodingBalanceChange) int {
		return cmp.Compare(a.BlockAccessIndex, b.BlockAccessIndex)
	}) {
		return errors.New("balance changes must be unique and sorted")
	}
	// check that the tx index is not greater than the max allowed for the block
	if len(e.BalanceChanges) > 0 && int(e.BalanceChanges[len(e.BalanceChanges)-1].BlockAccessIndex) > maxBALIndex {
		return fmt.Errorf("balance change index exceeds limit, index: %d, limit: %d", e.BalanceChanges[len(e.BalanceChanges)-1].BlockAccessIndex, maxBALIndex)
	}

	// Check the nonce changes are sorted in order, and unique by tx index
	if !isStrictlySortedFunc(e.NonceChanges, func(a, b encodingAccountNonce) int {
		return cmp.Compare(a.BlockAccessIndex, b.BlockAccessIndex)
	}) {
		return errors.New("nonce changes must be unique and sorted")
	}
	// check that the tx index of the highest nonce change is not greater than
	// the max allowed for the block
	if len(e.NonceChanges) > 0 && int(e.NonceChanges[len(e.NonceChanges)-1].BlockAccessIndex) > maxBALIndex {
		return fmt.Errorf("nonce change index exceeds limit, index: %d, limit: %d", e.NonceChanges[len(e.NonceChanges)-1].BlockAccessIndex, maxBALIndex)
	}

	// Check the code changes are sorted in order, and unique by tx index
	if !isStrictlySortedFunc(e.CodeChanges, func(a, b encodingCodeChange) int {
		return cmp.Compare(a.BlockAccessIndex, b.BlockAccessIndex)
	}) {
		return errors.New("code changes must be unique and sorted")
	}
	// check that the tx index of the highest code changeis not greater than the
	// max allowed for the block
	if len(e.CodeChanges) > 0 && int(e.CodeChanges[len(e.CodeChanges)-1].BlockAccessIndex) > maxBALIndex {
		return fmt.Errorf("code change index exceeds limit, index: %d, limit: %d", e.CodeChanges[len(e.CodeChanges)-1].BlockAccessIndex, maxBALIndex)
	}
	// Check that none of the code changes report a new code which is larger
	// than the max allowed by the protocol
	for _, change := range e.CodeChanges {
		if len(change.NewCode) > params.MaxCodeSizeAmsterdam {
			return errors.New("code change contained oversized code")
		}
	}
	return nil
}

// Copy returns a deep copy of the account access
func (e *AccountAccess) Copy() AccountAccess {
	res := AccountAccess{
		Address:        e.Address,
		StorageReads:   make([]*uint256.Int, 0, len(e.StorageReads)),
		BalanceChanges: make([]encodingBalanceChange, 0, len(e.BalanceChanges)),
		NonceChanges:   slices.Clone(e.NonceChanges),
		StorageChanges: make([]encodingSlotChanges, 0, len(e.StorageChanges)),
		CodeChanges:    make([]encodingCodeChange, 0, len(e.CodeChanges)),
	}
	for _, slot := range e.StorageReads {
		res.StorageReads = append(res.StorageReads, slot.Clone())
	}
	for _, change := range e.BalanceChanges {
		res.BalanceChanges = append(res.BalanceChanges, encodingBalanceChange{
			BlockAccessIndex: change.BlockAccessIndex,
			PostBalance:      change.PostBalance.Clone(),
		})
	}
	for _, slot := range e.StorageChanges {
		writes := make([]encodingStorageWrite, 0, len(slot.SlotChanges))
		for _, w := range slot.SlotChanges {
			writes = append(writes, encodingStorageWrite{
				BlockAccessIndex: w.BlockAccessIndex,
				PostValue:        w.PostValue.Clone(),
			})
		}
		res.StorageChanges = append(res.StorageChanges, encodingSlotChanges{
			Slot:        slot.Slot.Clone(),
			SlotChanges: writes,
		})
	}
	for _, codeChange := range e.CodeChanges {
		res.CodeChanges = append(res.CodeChanges, encodingCodeChange{
			BlockAccessIndex: codeChange.BlockAccessIndex,
			NewCode:          bytes.Clone(codeChange.NewCode),
		})
	}
	return res
}

// EncodeRLP returns the RLP-encoded access list
func (b *ConstructionBlockAccessList) EncodeRLP(wr io.Writer) error {
	return b.ToEncodingObj().EncodeRLP(wr)
}

var _ rlp.Encoder = &ConstructionBlockAccessList{}

// toEncodingObj creates an instance of the ConstructionAccountAccess of the type
// that is used as input for the encoding.
func (a *ConstructionAccountAccess) toEncodingObj(addr common.Address) AccountAccess {
	res := AccountAccess{
		Address:        addr,
		StorageChanges: make([]encodingSlotChanges, 0, len(a.StorageWrites)),
		StorageReads:   make([]*uint256.Int, 0, len(a.StorageReads)),
		BalanceChanges: make([]encodingBalanceChange, 0, len(a.BalanceChanges)),
		NonceChanges:   make([]encodingAccountNonce, 0, len(a.NonceChanges)),
		CodeChanges:    make([]encodingCodeChange, 0, len(a.CodeChange)),
	}

	// Convert write slots
	writeSlots := slices.Collect(maps.Keys(a.StorageWrites))
	slices.SortFunc(writeSlots, common.Hash.Cmp)
	for _, slot := range writeSlots {
		obj := encodingSlotChanges{
			Slot: new(uint256.Int).SetBytes(slot[:]),
		}
		slotWrites := a.StorageWrites[slot]
		obj.SlotChanges = make([]encodingStorageWrite, 0, len(slotWrites))

		indices := slices.Collect(maps.Keys(slotWrites))
		slices.Sort(indices)
		for _, index := range indices {
			val := slotWrites[index]
			obj.SlotChanges = append(obj.SlotChanges, encodingStorageWrite{
				BlockAccessIndex: index,
				PostValue:        new(uint256.Int).SetBytes(val[:]),
			})
		}
		res.StorageChanges = append(res.StorageChanges, obj)
	}

	// Convert read slots
	readSlots := slices.Collect(maps.Keys(a.StorageReads))
	slices.SortFunc(readSlots, common.Hash.Cmp)
	for _, slot := range readSlots {
		res.StorageReads = append(res.StorageReads, new(uint256.Int).SetBytes(slot[:]))
	}

	// Convert balance changes
	balanceIndices := slices.Collect(maps.Keys(a.BalanceChanges))
	slices.Sort(balanceIndices)
	for _, idx := range balanceIndices {
		res.BalanceChanges = append(res.BalanceChanges, encodingBalanceChange{
			BlockAccessIndex: idx,
			PostBalance:      a.BalanceChanges[idx].Clone(),
		})
	}

	// Convert nonce changes
	nonceIndices := slices.Collect(maps.Keys(a.NonceChanges))
	slices.Sort(nonceIndices)
	for _, idx := range nonceIndices {
		res.NonceChanges = append(res.NonceChanges, encodingAccountNonce{
			BlockAccessIndex: idx,
			PostNonce:        a.NonceChanges[idx],
		})
	}

	// Convert code change
	codeIndices := slices.Collect(maps.Keys(a.CodeChange))
	slices.Sort(codeIndices)
	for _, idx := range codeIndices {
		res.CodeChanges = append(res.CodeChanges, encodingCodeChange{
			BlockAccessIndex: idx,

			// TODO(rjl493456442) the contract code is not deep-copied.
			// In theory the deep-copy is unnecessary, the semantics of
			// the function should be probably changed that the returned
			// AccessList is unsafe for modification.
			NewCode: a.CodeChange[idx],
		})
	}
	return res
}

// ToEncodingObj returns an instance of the access list expressed as the type
// which is used as input for the encoding/decoding.
func (b *ConstructionBlockAccessList) ToEncodingObj() *BlockAccessList {
	var addresses []common.Address
	for addr := range b.Accounts {
		addresses = append(addresses, addr)
	}
	slices.SortFunc(addresses, common.Address.Cmp)

	res := BlockAccessList{Accounts: make([]AccountAccess, 0, len(addresses))}
	for _, addr := range addresses {
		res.Accounts = append(res.Accounts, b.Accounts[addr].toEncodingObj(addr))
	}
	// Referenced, not copied; the construction list is not reused after encoding.
	// Normalized empty->nil so encode/decode round-trips compare equal.
	if len(b.EmptyAccounts) > 0 {
		res.EmptyAccounts = b.EmptyAccounts
	}
	if len(b.EmptySlots) > 0 {
		res.EmptySlots = b.EmptySlots
	}
	return &res
}

func (e *BlockAccessList) PrettyPrint() string {
	var res bytes.Buffer
	printWithIndent := func(indent int, text string) {
		fmt.Fprintf(&res, "%s%s\n", strings.Repeat("    ", indent), text)
	}
	for _, accountDiff := range e.Accounts {
		printWithIndent(0, fmt.Sprintf("%x:", accountDiff.Address))
		printWithIndent(1, "storage changes:")
		for _, slot := range accountDiff.StorageChanges {
			printWithIndent(2, fmt.Sprintf("%s:", slot.Slot.Hex()))
			for _, access := range slot.SlotChanges {
				printWithIndent(3, fmt.Sprintf("%d: %s", access.BlockAccessIndex, access.PostValue.Hex()))
			}
		}
		printWithIndent(1, "storage reads:")
		for _, slot := range accountDiff.StorageReads {
			printWithIndent(2, slot.Hex())
		}
		printWithIndent(1, "balance changes:")
		for _, change := range accountDiff.BalanceChanges {
			printWithIndent(2, fmt.Sprintf("%d: %s", change.BlockAccessIndex, change.PostBalance))
		}
		printWithIndent(1, "nonce changes:")
		for _, change := range accountDiff.NonceChanges {
			printWithIndent(2, fmt.Sprintf("%d: %d", change.BlockAccessIndex, change.PostNonce))
		}
		printWithIndent(1, "code changes:")
		for _, change := range accountDiff.CodeChanges {
			printWithIndent(2, fmt.Sprintf("%d: %x", change.BlockAccessIndex, change.NewCode))
		}
	}
	return res.String()
}

// Copy returns a deep copy of the access list
func (e *BlockAccessList) Copy() *BlockAccessList {
	cpy := BlockAccessList{Accounts: make([]AccountAccess, 0, len(e.Accounts))}
	for _, accountAccess := range e.Accounts {
		cpy.Accounts = append(cpy.Accounts, accountAccess.Copy())
	}
	if e.EmptyAccounts != nil {
		cpy.EmptyAccounts = maps.Clone(e.EmptyAccounts)
	}
	if e.EmptySlots != nil {
		cpy.EmptySlots = make(map[common.Address]map[common.Hash]struct{}, len(e.EmptySlots))
		for addr, slots := range e.EmptySlots {
			cpy.EmptySlots[addr] = maps.Clone(slots)
		}
	}
	return &cpy
}
