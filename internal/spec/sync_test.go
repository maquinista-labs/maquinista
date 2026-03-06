package spec

import (
	"testing"
)

func TestSyncResult_Empty(t *testing.T) {
	r := &SyncResult{}
	if len(r.Created) != 0 || len(r.Updated) != 0 || len(r.Orphaned) != 0 || r.DepsSet != 0 {
		t.Error("expected empty SyncResult")
	}
}
