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
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/internal/testrand"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"
)

func makeTestConstructionBAL() *ConstructionBlockAccessList {
	return &ConstructionBlockAccessList{
		Accounts: map[common.Address]*ConstructionAccountAccess{
			common.BytesToAddress([]byte{0xff, 0xff}): {
				StorageWrites: map[common.Hash]map[uint32]common.Hash{
					common.BytesToHash([]byte{0x01}): {
						1: common.BytesToHash([]byte{1, 2, 3, 4}),
						2: common.BytesToHash([]byte{1, 2, 3, 4, 5, 6}),
					},
					common.BytesToHash([]byte{0x10}): {
						20: common.BytesToHash([]byte{1, 2, 3, 4}),
					},
				},
				StorageReads: map[common.Hash]struct{}{
					common.BytesToHash([]byte{1, 2, 3, 4, 5, 6, 7}): {},
				},
				BalanceChanges: map[uint32]*uint256.Int{
					1: uint256.NewInt(100),
					2: uint256.NewInt(500),
				},
				NonceChanges: map[uint32]uint64{
					1: 2,
					2: 6,
				},
				CodeChange: map[uint32][]byte{
					0: common.Hex2Bytes("deadbeef"),
				},
			},
			common.BytesToAddress([]byte{0xff, 0xff, 0xff}): {
				StorageWrites: map[common.Hash]map[uint32]common.Hash{
					common.BytesToHash([]byte{0x01}): {
						2: common.BytesToHash([]byte{1, 2, 3, 4, 5, 6}),
						3: common.BytesToHash([]byte{1, 2, 3, 4, 5, 6, 7, 8}),
					},
					common.BytesToHash([]byte{0x10}): {
						21: common.BytesToHash([]byte{1, 2, 3, 4, 5}),
					},
				},
				StorageReads: map[common.Hash]struct{}{
					common.BytesToHash([]byte{1, 2, 3, 4, 5, 6, 7, 8}): {},
				},
				BalanceChanges: map[uint32]*uint256.Int{
					2: uint256.NewInt(100),
					3: uint256.NewInt(500),
				},
				NonceChanges: map[uint32]uint64{
					1: 2,
				},
				CodeChange: map[uint32][]byte{
					0: common.Hex2Bytes("deadbeef"),
				},
			},
		},
	}
}

// TestBALEncoding tests that a populated access list can be encoded/decoded correctly.
func TestBALEncoding(t *testing.T) {
	var buf bytes.Buffer
	bal := makeTestConstructionBAL()
	err := bal.EncodeRLP(&buf)
	if err != nil {
		t.Fatalf("encoding failed: %v\n", err)
	}
	var dec BlockAccessList
	if err := dec.DecodeRLP(rlp.NewStream(bytes.NewReader(buf.Bytes()), 0)); err != nil {
		t.Fatalf("decoding failed: %v\n", err)
	}
	if dec.Hash() != bal.ToEncodingObj().Hash() {
		t.Fatalf("encoded block hash doesn't match decoded")
	}
	if !reflect.DeepEqual(bal.ToEncodingObj(), &dec) {
		t.Fatal("decoded BAL doesn't match")
	}
}

// TestBALEmptinessRoundtrip builds a BAL through the construction API with a mix
// of empty and existing accounts/slots — including slots that were empty at block
// start and then created — and asserts the emptiness signal survives encode/decode
// via the two bitmaps.
func TestBALEmptinessRoundtrip(t *testing.T) {
	var (
		addrEmpty = common.BytesToAddress([]byte{0x01}) // read-only, empty at block start
		addrExist = common.BytesToAddress([]byte{0x02}) // read-only, exists at block start
		addrSlots = common.BytesToAddress([]byte{0x03}) // carries the slot mix

		slotReadEmpty = common.BytesToHash([]byte{0x10}) // read-only, zero at start
		slotReadExist = common.BytesToHash([]byte{0x11}) // read-only, nonzero at start
		slotMadeEmpty = common.BytesToHash([]byte{0x12}) // zero at start, then written (created)
		slotMadeExist = common.BytesToHash([]byte{0x13}) // nonzero at start, then written
	)

	b := NewConstructionBlockAccessList()
	// Two bare reads: one resolves empty, one resolves existing.
	b.AccountRead(addrEmpty)
	b.AccountEmpty(addrEmpty)
	b.AccountRead(addrExist)
	// Slot mix on a third account.
	b.StorageRead(addrSlots, slotReadEmpty)
	b.SlotEmpty(addrSlots, slotReadEmpty)
	b.StorageRead(addrSlots, slotReadExist)
	b.SlotEmpty(addrSlots, slotMadeEmpty) // observed empty at first read...
	b.StorageWrite(1, addrSlots, slotMadeEmpty, common.BytesToHash([]byte{0x99}))
	b.StorageWrite(1, addrSlots, slotMadeExist, common.BytesToHash([]byte{0xAA}))

	var buf bytes.Buffer
	if err := b.EncodeRLP(&buf); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var dec BlockAccessList
	if err := rlp.DecodeBytes(buf.Bytes(), &dec); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The emptiness sets must survive the round-trip exactly.
	enc := b.ToEncodingObj()
	if !reflect.DeepEqual(enc.EmptyAccounts, dec.EmptyAccounts) {
		t.Fatalf("empty accounts mismatch:\n got %+v\nwant %+v", dec.EmptyAccounts, enc.EmptyAccounts)
	}
	if !reflect.DeepEqual(enc.EmptySlots, dec.EmptySlots) {
		t.Fatalf("empty slots mismatch:\n got %+v\nwant %+v", dec.EmptySlots, enc.EmptySlots)
	}

	if _, ok := dec.EmptyAccounts[addrEmpty]; !ok {
		t.Error("addrEmpty: expected empty account")
	}
	if _, ok := dec.EmptyAccounts[addrExist]; ok {
		t.Error("addrExist: expected not marked empty")
	}
	isEmpty := func(slot common.Hash) bool {
		_, ok := dec.EmptySlots[addrSlots][slot]
		return ok
	}
	for _, tc := range []struct {
		slot common.Hash
		want bool
		name string
	}{
		{slotReadEmpty, true, "slotReadEmpty"},
		{slotReadExist, false, "slotReadExist"},
		{slotMadeEmpty, true, "slotMadeEmpty (created from zero)"},
		{slotMadeExist, false, "slotMadeExist (written over nonzero)"},
	} {
		if got := isEmpty(tc.slot); got != tc.want {
			t.Errorf("%s: empty=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// encodeBALRaw assembles the wire form [accounts, acctBM, slotBM] with
// caller-chosen bitmap bytes, so a test can feed DecodeRLP a mismatched bitmap.
func encodeBALRaw(t *testing.T, accounts []AccountAccess, acctBM, slotBM []byte) []byte {
	t.Helper()
	var w bytes.Buffer
	buf := rlp.NewEncoderBuffer(&w)
	outer := buf.List()
	inner := buf.List()
	for i := range accounts {
		if err := accounts[i].EncodeRLP(buf); err != nil {
			t.Fatalf("encode account: %v", err)
		}
	}
	buf.ListEnd(inner)
	buf.WriteBytes(acctBM)
	buf.WriteBytes(slotBM)
	buf.ListEnd(outer)
	if err := buf.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return w.Bytes()
}

// TestBALEmptinessBitmapSizeMismatch locks the DecodeRLP guards: a wire BAL
// whose emptiness bitmaps don't match the account/slot counts must be rejected.
// A wrong-sized bitmap means the bits no longer describe the listed items — a
// consensus-critical rejection, not a best-effort parse.
func TestBALEmptinessBitmapSizeMismatch(t *testing.T) {
	src := makeTestBAL(true)
	accounts := src.Accounts
	acctBytes := bitmapLen(len(accounts))
	slotBytes := bitmapLen(src.slotCount())

	// Sanity: correctly-sized (all-zero) bitmaps decode without error.
	if err := rlp.DecodeBytes(encodeBALRaw(t, accounts, make([]byte, acctBytes), make([]byte, slotBytes)), new(BlockAccessList)); err != nil {
		t.Fatalf("correctly-sized bitmaps must decode: %v", err)
	}

	for _, tc := range []struct {
		name           string
		acctBM, slotBM []byte
	}{
		{"account bitmap too long", make([]byte, acctBytes+1), make([]byte, slotBytes)},
		{"account bitmap too short", make([]byte, acctBytes-1), make([]byte, slotBytes)},
		{"slot bitmap too long", make([]byte, acctBytes), make([]byte, slotBytes+1)},
		{"slot bitmap too short", make([]byte, acctBytes), make([]byte, slotBytes-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := rlp.DecodeBytes(encodeBALRaw(t, accounts, tc.acctBM, tc.slotBM), new(BlockAccessList))
			if err == nil {
				t.Fatal("expected size-mismatch error, got nil")
			}
			if !strings.Contains(err.Error(), "bitmap size mismatch") {
				t.Fatalf("expected bitmap size mismatch error, got: %v", err)
			}
		})
	}
}

// TestEmptyBitmapPacking locks the consensus-critical LSB-first bit order.
func TestEmptyBitmapPacking(t *testing.T) {
	bm := make([]byte, bitmapLen(10))
	if len(bm) != 2 {
		t.Fatalf("bitmapLen(10) = %d bytes, want 2", len(bm))
	}
	setBit(bm, 0)
	setBit(bm, 3)
	setBit(bm, 9)
	// bit0,bit3 -> byte0 = 0b0000_1001 = 0x09; bit9 -> byte1 = 0b0000_0010 = 0x02.
	if bm[0] != 0x09 || bm[1] != 0x02 {
		t.Fatalf("packed bitmap = %#x, want [0x09 0x02]", bm)
	}
	for i, want := range map[int]bool{0: true, 1: false, 3: true, 8: false, 9: true} {
		if getBit(bm, i) != want {
			t.Errorf("getBit(%d) = %v, want %v", i, getBit(bm, i), want)
		}
	}
}

func TestConstructionBALMerge(t *testing.T) {
	var (
		addrA = common.BytesToAddress([]byte{0xAA})
		addrB = common.BytesToAddress([]byte{0xBB})
		slot1 = common.BytesToHash([]byte{0x01})
		slot2 = common.BytesToHash([]byte{0x02})
		slot3 = common.BytesToHash([]byte{0x03})
	)
	a := NewConstructionBlockAccessList()
	a.StorageWrite(1, addrA, slot1, common.BytesToHash([]byte{0x11}))
	a.StorageRead(addrA, slot2) // demoted by other's write below
	a.BalanceChange(1, addrA, uint256.NewInt(100))
	a.NonceChange(addrA, 1, 7)

	b := NewConstructionBlockAccessList()
	b.StorageWrite(2, addrA, slot1, common.BytesToHash([]byte{0x22})) // same slot, disjoint txIdx
	b.StorageWrite(2, addrA, slot2, common.BytesToHash([]byte{0x33}))
	b.StorageRead(addrA, slot3)
	b.BalanceChange(2, addrA, uint256.NewInt(200))
	b.NonceChange(addrA, 2, 8)
	b.CodeChange(addrB, 2, []byte{0xde, 0xad}) // account only in other

	a.Merge(b)

	accA := a.Accounts[addrA]
	wantWrites := map[common.Hash]map[uint32]common.Hash{
		slot1: {1: common.BytesToHash([]byte{0x11}), 2: common.BytesToHash([]byte{0x22})},
		slot2: {2: common.BytesToHash([]byte{0x33})},
	}
	if !reflect.DeepEqual(accA.StorageWrites, wantWrites) {
		t.Fatalf("storage writes mismatch: got %v, want %v", accA.StorageWrites, wantWrites)
	}
	wantReads := map[common.Hash]struct{}{slot3: {}}
	if !reflect.DeepEqual(accA.StorageReads, wantReads) {
		t.Fatalf("storage reads mismatch: got %v, want %v", accA.StorageReads, wantReads)
	}
	if accA.BalanceChanges[1].Uint64() != 100 || accA.BalanceChanges[2].Uint64() != 200 {
		t.Fatalf("balance changes mismatch: %v", accA.BalanceChanges)
	}
	if accA.NonceChanges[1] != 7 || accA.NonceChanges[2] != 8 {
		t.Fatalf("nonce changes mismatch: %v", accA.NonceChanges)
	}
	accB, ok := a.Accounts[addrB]
	if !ok {
		t.Fatal("account only present in other was not adopted")
	}
	if !bytes.Equal(accB.CodeChange[2], []byte{0xde, 0xad}) {
		t.Fatalf("code change for adopted account missing: %x", accB.CodeChange[2])
	}
}

func makeTestAccountAccess(sort bool) AccountAccess {
	var (
		storageWrites []encodingSlotChanges
		storageReads  []*uint256.Int
		balances      []encodingBalanceChange
		nonces        []encodingAccountNonce
		codes         []encodingCodeChange
	)
	randSlot := func() *uint256.Int {
		return new(uint256.Int).SetBytes(testrand.Bytes(32))
	}
	for i := 0; i < 5; i++ {
		slot := encodingSlotChanges{
			Slot: randSlot(),
		}
		for j := 0; j < 3; j++ {
			slot.SlotChanges = append(slot.SlotChanges, encodingStorageWrite{
				BlockAccessIndex: uint32(2 * j),
				PostValue:        randSlot(),
			})
		}
		if sort {
			slices.SortFunc(slot.SlotChanges, func(a, b encodingStorageWrite) int {
				return cmp.Compare(a.BlockAccessIndex, b.BlockAccessIndex)
			})
		}
		storageWrites = append(storageWrites, slot)
	}
	if sort {
		slices.SortFunc(storageWrites, func(a, b encodingSlotChanges) int {
			return a.Slot.Cmp(b.Slot)
		})
	}

	for i := 0; i < 5; i++ {
		storageReads = append(storageReads, randSlot())
	}
	if sort {
		slices.SortFunc(storageReads, func(a, b *uint256.Int) int {
			return a.Cmp(b)
		})
	}

	for i := 0; i < 5; i++ {
		balances = append(balances, encodingBalanceChange{
			BlockAccessIndex: uint32(2 * i),
			PostBalance:      new(uint256.Int).SetBytes(testrand.Bytes(16)),
		})
	}
	if sort {
		slices.SortFunc(balances, func(a, b encodingBalanceChange) int {
			return cmp.Compare(a.BlockAccessIndex, b.BlockAccessIndex)
		})
	}

	for i := 0; i < 5; i++ {
		nonces = append(nonces, encodingAccountNonce{
			BlockAccessIndex: uint32(2 * i),
			PostNonce:        uint64(i + 100),
		})
	}
	if sort {
		slices.SortFunc(nonces, func(a, b encodingAccountNonce) int {
			return cmp.Compare(a.BlockAccessIndex, b.BlockAccessIndex)
		})
	}

	for i := 0; i < 5; i++ {
		codes = append(codes, encodingCodeChange{
			BlockAccessIndex: uint32(2 * i),
			NewCode:          testrand.Bytes(256),
		})
	}
	if sort {
		slices.SortFunc(codes, func(a, b encodingCodeChange) int {
			return cmp.Compare(a.BlockAccessIndex, b.BlockAccessIndex)
		})
	}

	return AccountAccess{
		Address:        common.Address(testrand.Bytes(20)),
		StorageChanges: storageWrites,
		StorageReads:   storageReads,
		BalanceChanges: balances,
		NonceChanges:   nonces,
		CodeChanges:    codes,
	}
}

func makeTestBAL(sort bool) *BlockAccessList {
	list := BlockAccessList{Accounts: make([]AccountAccess, 0, 5)}
	for i := 0; i < 5; i++ {
		list.Accounts = append(list.Accounts, makeTestAccountAccess(sort))
	}
	if sort {
		slices.SortFunc(list.Accounts, func(a, b AccountAccess) int {
			return bytes.Compare(a.Address[:], b.Address[:])
		})
	}
	return &list
}

func TestBlockAccessListCopy(t *testing.T) {
	list := makeTestBAL(true)
	cpy := list.Copy()
	cpyCpy := cpy.Copy()

	if !reflect.DeepEqual(list, cpy) {
		t.Fatal("block access mismatch")
	}
	if !reflect.DeepEqual(cpy, cpyCpy) {
		t.Fatal("block access mismatch")
	}

	// Make sure the mutations on copy won't affect the origin
	for _, aa := range cpyCpy.Accounts {
		for i := 0; i < len(aa.StorageReads); i++ {
			aa.StorageReads[i] = new(uint256.Int).SetBytes(testrand.Bytes(32))
		}
	}
	if !reflect.DeepEqual(list, cpy) {
		t.Fatal("block access mismatch")
	}
}

func TestBlockAccessListItemCount(t *testing.T) {
	empty := &BlockAccessList{}
	if got := empty.itemCount(); got != 0 {
		t.Fatalf("empty BAL item count: got %d, want 0", got)
	}

	addr1 := common.Address(testrand.Bytes(20))
	addr2 := common.Address(testrand.Bytes(20))
	one := func() *uint256.Int { return new(uint256.Int).SetBytes(testrand.Bytes(32)) }
	bal := &BlockAccessList{Accounts: []AccountAccess{
		{
			Address: addr1,
			StorageChanges: []encodingSlotChanges{
				{Slot: one(), SlotChanges: []encodingStorageWrite{{BlockAccessIndex: 0, PostValue: one()}, {BlockAccessIndex: 1, PostValue: one()}}},
				{Slot: one()},
			},
			StorageReads: []*uint256.Int{one()},
		},
		{Address: addr2}, // address-only, no slots
	}}
	// 2 addresses + 2 write-slots + 1 read-slot = 5 items.
	// (Multiple TxIdx writes to the same slot count as ONE item.)
	if got := bal.itemCount(); got != 5 {
		t.Fatalf("item count: got %d, want 5", got)
	}
}

func TestBlockAccessListValidateSize(t *testing.T) {
	// Build a BAL with exactly 30 items: 3 addresses, each with 9 storage
	// slots (some writes, some reads). 3 + 9*3 = 30.
	one := func() *uint256.Int { return new(uint256.Int).SetBytes(testrand.Bytes(32)) }
	bal := BlockAccessList{Accounts: make([]AccountAccess, 3)}
	for i := range bal.Accounts {
		bal.Accounts[i].Address = common.Address(testrand.Bytes(20))
		for j := 0; j < 5; j++ {
			bal.Accounts[i].StorageChanges = append(bal.Accounts[i].StorageChanges, encodingSlotChanges{
				Slot: one(), SlotChanges: []encodingStorageWrite{{BlockAccessIndex: 0, PostValue: one()}},
			})
		}
		for j := 0; j < 4; j++ {
			bal.Accounts[i].StorageReads = append(bal.Accounts[i].StorageReads, one())
		}
	}
	if got := bal.itemCount(); got != 30 {
		t.Fatalf("setup: item count = %d, want 30", got)
	}

	// limit = blockGasLimit / BALItemCost.
	// 30 items requires limit >= 30, i.e. gasLimit >= 30 * 2000 = 60_000.
	tests := []struct {
		name        string
		gasLimit    uint64
		expectError bool
	}{
		{"exactly at limit", 30 * params.BALItemCost, false},
		{"well above limit", 60_000_000, false},
		{"one below limit", 30*params.BALItemCost - 1, true},
		{"zero gas limit", 0, true},
	}
	for _, tc := range tests {
		err := bal.ValidateSize(tc.gasLimit)
		if (err != nil) != tc.expectError {
			t.Errorf("%s: got err=%v, expectError=%v", tc.name, err, tc.expectError)
		}
	}

	// Empty BAL is always valid (even with 0 gas limit).
	if err := (&BlockAccessList{}).ValidateSize(0); err != nil {
		t.Fatalf("empty BAL must pass any limit: %v", err)
	}
}

func TestBlockAccessListValidation(t *testing.T) {
	// Validate the block access list after RLP decoding
	enc := makeTestBAL(true)
	if err := enc.Validate(math.MaxUint64, 10000); err != nil {
		t.Fatalf("Unexpected validation error: %v", err)
	}
	var buf bytes.Buffer
	if err := enc.EncodeRLP(&buf); err != nil {
		t.Fatalf("Unexpected encoding error: %v", err)
	}

	var dec BlockAccessList
	if err := dec.DecodeRLP(rlp.NewStream(bytes.NewReader(buf.Bytes()), 0)); err != nil {
		t.Fatalf("Unexpected RLP-decode error: %v", err)
	}
	if err := dec.Validate(math.MaxUint64, 10000); err != nil {
		t.Fatalf("Unexpected validation error: %v", err)
	}

	// Validate the derived block access list
	cBAL := makeTestConstructionBAL()
	listB := cBAL.ToEncodingObj()
	if err := listB.Validate(math.MaxUint64, 10000); err != nil {
		t.Fatalf("Unexpected validation error: %v", err)
	}
}
