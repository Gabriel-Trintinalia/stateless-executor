package main

import (
	"encoding/json"
	"testing"

	"github.com/Gabriel-Trintinalia/stateless-executor/fixture"
)

// The union keys are capitalised. buildTx in fixture/encode.go errors on any
// other spelling, so these are the only keys a fixture that encodes can carry —
// which is what makes the lowercase lookups this replaces provably dead.
func TestExtractBlockInfoCountsByTxType(t *testing.T) {
	tx := func(key string) fixture.FixtureTx {
		return fixture.FixtureTx{
			Transaction: map[string]json.RawMessage{key: json.RawMessage(`{}`)},
		}
	}

	var f fixture.FixtureFile
	f.StatelessInput.Block.Header.GasUsed = 25658526
	f.StatelessInput.Block.Body.Transactions = []fixture.FixtureTx{
		tx("Legacy"),
		tx("Eip1559"), tx("Eip1559"),
		tx("Eip2930"),
		tx("Eip4844"),
		tx("Eip7702"),
	}

	got := extractBlockInfo(&f)
	if got.TxCount != 6 {
		t.Errorf("TxCount = %d, want 6", got.TxCount)
	}
	if got.GasUsed != 25658526 {
		t.Errorf("GasUsed = %d", got.GasUsed)
	}
	if got.LegacyTxs != 1 {
		t.Errorf("LegacyTxs = %d, want 1", got.LegacyTxs)
	}
	if got.Eip1559Txs != 2 {
		t.Errorf("Eip1559Txs = %d, want 2", got.Eip1559Txs)
	}
	if got.Eip2930Txs != 1 || got.Eip4844Txs != 1 || got.Eip7702Txs != 1 {
		t.Errorf("2930/4844/7702 = %d/%d/%d, want 1/1/1", got.Eip2930Txs, got.Eip4844Txs, got.Eip7702Txs)
	}
	// The whole point: typed transactions must no longer land in the legacy
	// bucket.
	if got.LegacyTxs == got.TxCount {
		t.Error("every transaction was counted as legacy — the casing regression is back")
	}
}

// A lowercase key is not a real fixture spelling; if one ever appears it falls
// to legacy rather than being silently miscounted as a type.
func TestExtractBlockInfoUnknownKeyIsLegacy(t *testing.T) {
	var f fixture.FixtureFile
	f.StatelessInput.Block.Body.Transactions = []fixture.FixtureTx{
		{Transaction: map[string]json.RawMessage{"eip1559": json.RawMessage(`{}`)}},
	}
	got := extractBlockInfo(&f)
	if got.LegacyTxs != 1 || got.Eip1559Txs != 0 {
		t.Errorf("got %+v, want the unknown key counted as legacy", got)
	}
}
