// Copyright 2021 The go-ethereum Authors
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

import "github.com/ethereum/go-ethereum/metrics"

var (
	accountReadMeters        = metrics.NewRegisteredMeter("state/read/account", nil)
	storageReadMeters        = metrics.NewRegisteredMeter("state/read/storage", nil)
	accountUpdatedMeter      = metrics.NewRegisteredMeter("state/update/account", nil)
	storageUpdatedMeter      = metrics.NewRegisteredMeter("state/update/storage", nil)
	accountDeletedMeter      = metrics.NewRegisteredMeter("state/delete/account", nil)
	storageDeletedMeter      = metrics.NewRegisteredMeter("state/delete/storage", nil)
	accountTrieUpdatedMeter  = metrics.NewRegisteredMeter("state/update/accountnodes", nil)
	storageTriesUpdatedMeter = metrics.NewRegisteredMeter("state/update/storagenodes", nil)
	accountTrieDeletedMeter  = metrics.NewRegisteredMeter("state/delete/accountnodes", nil)
	storageTriesDeletedMeter = metrics.NewRegisteredMeter("state/delete/storagenodes", nil)

	// Door-to-door read latency split by outcome: an empty result is a read a
	// BAL empty-skip would delete outright, so its mean is the per-skip saving;
	// exist is the comparison baseline.
	accountReadEmptyTimer = metrics.NewRegisteredResettingTimer("state/read/account/empty/duration", nil)
	accountReadExistTimer = metrics.NewRegisteredResettingTimer("state/read/account/exist/duration", nil)
	storageReadEmptyTimer = metrics.NewRegisteredResettingTimer("state/read/storage/empty/duration", nil)
	storageReadExistTimer = metrics.NewRegisteredResettingTimer("state/read/storage/exist/duration", nil)
)
