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
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/holiman/uint256"
)

// Each database read must land in the outcome timer matching what it returned:
// a missing account / zero slot in the empty timer, data in the exist timer.
func TestReadTimersSplitByOutcome(t *testing.T) {
	var (
		db       = NewDatabaseForTesting()
		existing = common.Address{0x01}
		missing  = common.Address{0xff}
		setKey   = common.Hash{0x01}
		unsetKey = common.Hash{0x99}
	)
	setup, _ := New(types.EmptyRootHash, db)
	setup.SetBalance(existing, uint256.NewInt(1), tracing.BalanceChangeUnspecified)
	setup.SetState(existing, setKey, common.Hash{0x02})
	root, err := setup.Commit(0, false, false)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	metrics.Enable() // timers no-op otherwise; production sets this via --metrics

	// Drain whatever earlier tests left behind.
	accountReadEmptyTimer.Snapshot()
	accountReadExistTimer.Snapshot()
	storageReadEmptyTimer.Snapshot()
	storageReadExistTimer.Snapshot()

	state, _ := New(root, db)
	state.GetBalance(existing)         // account, exists
	state.GetBalance(missing)          // account, empty
	state.GetState(existing, setKey)   // slot, exists
	state.GetState(existing, unsetKey) // slot, empty

	for _, c := range []struct {
		name  string
		count int
		want  int
	}{
		{"account/exist", accountReadExistTimer.Snapshot().Count(), 1},
		{"account/empty", accountReadEmptyTimer.Snapshot().Count(), 1},
		{"storage/exist", storageReadExistTimer.Snapshot().Count(), 1},
		{"storage/empty", storageReadEmptyTimer.Snapshot().Count(), 1},
	} {
		if c.count != c.want {
			t.Errorf("%s timer: %d samples, want %d", c.name, c.count, c.want)
		}
	}
}
