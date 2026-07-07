// Copyright 2026 The go-ethereum Authors
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

package state

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/types/bal"
	"github.com/ethereum/go-ethereum/internal/testrand"
)

type countingStateReader struct {
	accounts map[common.Address]int
	storages map[common.Address]map[common.Hash]int
	lock     sync.Mutex
}

func newRefStateReader() *countingStateReader {
	return &countingStateReader{
		accounts: make(map[common.Address]int),
		storages: make(map[common.Address]map[common.Hash]int),
	}
}

func (r *countingStateReader) validate(total int) error {
	var sum int
	for addr, n := range r.accounts {
		if n != 1 {
			return fmt.Errorf("duplicated account access: %x-%d", addr, n)
		}
		sum += 1

		slots, exists := r.storages[addr]
		if !exists {
			continue
		}
		for key, n := range slots {
			if n != 1 {
				return fmt.Errorf("duplicated storage access: %x-%x-%d", addr, key, n)
			}
			sum += 1
		}
	}
	for addr := range r.storages {
		_, exists := r.accounts[addr]
		if !exists {
			return fmt.Errorf("dangling storage access: %x", addr)
		}
	}
	if sum != total {
		return fmt.Errorf("unexpected number of access, want: %d, got: %d", total, sum)
	}
	return nil
}

func (r *countingStateReader) Account(addr common.Address) (*types.StateAccount, error) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.accounts[addr] += 1
	return nil, nil
}
func (r *countingStateReader) Storage(addr common.Address, slot common.Hash) (common.Hash, error) {
	r.lock.Lock()
	defer r.lock.Unlock()

	slots, exists := r.storages[addr]
	if !exists {
		slots = make(map[common.Hash]int)
		r.storages[addr] = slots
	}
	slots[slot] += 1
	return common.Hash{}, nil
}

func makeFetchTasks(n int) ([]*fetchTask, int) {
	var (
		total int
		tasks []*fetchTask
	)
	for i := 0; i < n; i++ {
		var slots []common.Hash
		if rand.Intn(3) != 0 {
			for j := 0; j < rand.Intn(100); j++ {
				slots = append(slots, testrand.Hash())
			}
		}
		tasks = append(tasks, &fetchTask{
			addr:  testrand.Address(),
			slots: slots,
		})
		total += len(slots) + 1
	}
	return tasks, total
}

func TestPrefetchReader(t *testing.T) {
	type suite struct {
		tasks   []*fetchTask
		threads int
		total   int
	}
	var suites []suite
	for i := 0; i < 100; i++ {
		tasks, total := makeFetchTasks(100)
		suites = append(suites, suite{
			tasks:   tasks,
			threads: rand.Intn(30) + 1,
			total:   total,
		})
	}
	// num(tasks) < num(threads)
	tasks, total := makeFetchTasks(1)
	suites = append(suites, suite{
		tasks:   tasks,
		threads: 100,
		total:   total,
	})
	for _, s := range suites {
		r := newRefStateReader()
		pr := newPrefetchStateReaderInternal(r, s.tasks, s.threads)
		pr.Wait()
		if err := r.validate(s.total); err != nil {
			t.Fatal(err)
		}
	}
}

func (r *countingStateReader) accountReads(addr common.Address) int {
	r.lock.Lock()
	defer r.lock.Unlock()
	return r.accounts[addr]
}

func (r *countingStateReader) storageReads(addr common.Address, slot common.Hash) int {
	r.lock.Lock()
	defer r.lock.Unlock()
	return r.storages[addr][slot]
}

// presenceStateReader is a countingStateReader that reports the configured
// accounts as existing, so statedb-level reads proceed to their storage.
type presenceStateReader struct {
	*countingStateReader
	present map[common.Address]*types.StateAccount
}

func (r *presenceStateReader) Account(addr common.Address) (*types.StateAccount, error) {
	r.countingStateReader.Account(addr)
	if acct := r.present[addr]; acct != nil {
		return acct.Copy(), nil
	}
	return nil, nil
}

// TestEmptySkipReaderCoversAllConsumers proves the skip placement: a key the
// access list flags as empty at block start never reaches the base reader,
// regardless of which consumer issued the read — the per-transaction execution
// reader, the plain statedb (the system-call path), or a prefetch worker.
// Unflagged keys must reach the base through every path, proving the
// assertions are not vacuous.
func TestEmptySkipReaderCoversAllConsumers(t *testing.T) {
	var (
		emptyAddr    = common.Address{0x01} // flagged empty at block start
		existAddr    = common.Address{0x02}
		contractAddr = common.Address{0x03}
		emptySlot    = common.Hash{0xaa} // flagged zero at block start
		existSlot    = common.Hash{0xbb}
	)
	prepared := bal.NewAccessListReader(bal.BlockAccessList{
		EmptyAccounts: map[common.Address]struct{}{emptyAddr: {}},
		EmptySlots:    map[common.Address]map[common.Hash]struct{}{contractAddr: {emptySlot: {}}},
	})
	newBase := func() *presenceStateReader {
		exist := types.NewEmptyStateAccount()
		exist.Nonce = 1
		return &presenceStateReader{
			countingStateReader: newRefStateReader(),
			present: map[common.Address]*types.StateAccount{
				existAddr:    exist,
				contractAddr: exist,
			},
		}
	}
	// newStack assembles the production reader layering over a recording base.
	newStack := func() (*presenceStateReader, Reader) {
		base := newBase()
		pr := newPrefetchStateReaderInternal(newEmptySkipReader(base, prepared), nil, 1)
		return base, newReaderWithPrefetch(nil, pr, pr)
	}
	// checkBase asserts flagged keys never reached the base and unflagged
	// ones did.
	checkBase := func(t *testing.T, base *presenceStateReader, consumer string) {
		t.Helper()
		if n := base.accountReads(emptyAddr); n != 0 {
			t.Errorf("%s: flagged-empty account reached the base reader %d times", consumer, n)
		}
		if n := base.storageReads(contractAddr, emptySlot); n != 0 {
			t.Errorf("%s: flagged-empty slot reached the base reader %d times", consumer, n)
		}
		if n := base.accountReads(existAddr); n == 0 {
			t.Errorf("%s: unflagged account never reached the base reader (vacuous test)", consumer)
		}
		if n := base.storageReads(contractAddr, existSlot); n == 0 {
			t.Errorf("%s: unflagged slot never reached the base reader (vacuous test)", consumer)
		}
	}

	// A BAL with no emptiness must not install the wrapper.
	if base := newRefStateReader(); newEmptySkipReader(base, bal.NewAccessListReader(bal.BlockAccessList{})) != StateReader(base) {
		t.Fatal("skip reader installed for an access list without emptiness")
	}

	t.Run("per-tx execution reader", func(t *testing.T) {
		base, rd := newStack()
		perTx := NewReaderWithAccessList(rd, prepared, 1)

		if acct, err := perTx.Account(emptyAddr); err != nil || acct != nil {
			t.Fatalf("flagged-empty account: want (nil, nil), got (%v, %v)", acct, err)
		}
		if acct, err := perTx.Account(existAddr); err != nil || acct == nil {
			t.Fatalf("unflagged account: want existing account, got (%v, %v)", acct, err)
		}
		if val, err := perTx.Storage(contractAddr, emptySlot); err != nil || val != (common.Hash{}) {
			t.Fatalf("flagged-empty slot: want zero, got (%v, %v)", val, err)
		}
		if _, err := perTx.Storage(contractAddr, existSlot); err != nil {
			t.Fatalf("unflagged slot: %v", err)
		}
		checkBase(t, base, "per-tx reader")
	})

	t.Run("plain statedb (system-call path)", func(t *testing.T) {
		base, rd := newStack()
		sdb, err := NewWithReader(types.EmptyRootHash, NewDatabaseForTesting(), rd)
		if err != nil {
			t.Fatal(err)
		}
		if bal := sdb.GetBalance(emptyAddr); !bal.IsZero() {
			t.Fatalf("flagged-empty account: want zero balance, got %v", bal)
		}
		sdb.GetBalance(existAddr)
		if val := sdb.GetState(contractAddr, emptySlot); val != (common.Hash{}) {
			t.Fatalf("flagged-empty slot: want zero, got %v", val)
		}
		sdb.GetState(contractAddr, existSlot)
		checkBase(t, base, "plain statedb")
	})

	t.Run("prefetch worker", func(t *testing.T) {
		base := newBase()
		pr := newPrefetchStateReaderInternal(newEmptySkipReader(base, prepared), []*fetchTask{
			{addr: emptyAddr},
			{addr: existAddr},
			{addr: contractAddr, slots: []common.Hash{emptySlot, existSlot}},
		}, 2)
		pr.Wait()
		checkBase(t, base, "prefetch worker")
	})
}
