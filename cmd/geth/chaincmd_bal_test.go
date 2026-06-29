// Copyright 2024 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/params"
)

// TestEnableAmsterdamForBAL verifies the `export --with-bal` Amsterdam switch:
// a config that is not on Amsterdam becomes Amsterdam-active from block 0, with a
// non-nil Amsterdam blob schedule (the IsAmsterdam paths nil-deref otherwise).
func TestEnableAmsterdamForBAL(t *testing.T) {
	// A London-active config with no Amsterdam fork scheduled.
	cfg := &params.ChainConfig{
		LondonBlock: big.NewInt(0),
		BlobScheduleConfig: &params.BlobScheduleConfig{
			Osaka: params.DefaultOsakaBlobConfig,
		},
	}

	num, time := big.NewInt(1), uint64(1)
	if cfg.IsAmsterdam(num, time) {
		t.Fatal("precondition: config should not be on Amsterdam before the flip")
	}

	enableAmsterdamForBAL(cfg)

	if !cfg.IsAmsterdam(num, time) {
		t.Error("config is not on Amsterdam after enableAmsterdamForBAL")
	}
	if cfg.AmsterdamTime == nil || *cfg.AmsterdamTime != 0 {
		t.Errorf("AmsterdamTime = %v, want 0", cfg.AmsterdamTime)
	}
	if cfg.BlobScheduleConfig.Amsterdam == nil {
		t.Error("Amsterdam blob schedule is nil; IsAmsterdam paths would nil-deref")
	}
}

// TestEnableAmsterdamForBAL_NilBlobSchedule ensures the helper is safe when the
// source config carries no blob schedule at all.
func TestEnableAmsterdamForBAL_NilBlobSchedule(t *testing.T) {
	cfg := &params.ChainConfig{LondonBlock: big.NewInt(0)}

	enableAmsterdamForBAL(cfg)

	if !cfg.IsAmsterdam(big.NewInt(1), 1) {
		t.Error("config is not on Amsterdam after the flip")
	}
	if cfg.BlobScheduleConfig == nil || cfg.BlobScheduleConfig.Amsterdam == nil {
		t.Error("Amsterdam blob schedule was not populated")
	}
}
