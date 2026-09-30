package monitor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maquinista-labs/maquinista/internal/state"
)

// writePiSessionFile creates a fake pi transcript whose NAME embeds the
// given UTC creation time and whose first line is a parseable v3 header.
// The filename matches pi's real convention, millis separator '-'.
func writePiSessionFile(t *testing.T, dir string, created time.Time, id string) string {
	t.Helper()
	name := created.UTC().Format("2006-01-02T15-04-05.000Z")
	name = name[:strings.LastIndex(name, ".")] + "-" + name[strings.LastIndex(name, ".")+1:]
	name += "_" + id + ".jsonl"
	header := fmt.Sprintf(`{"type":"session","version":3,"id":%q,"cwd":%q}`+"\n", id, dir)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(header), 0o644); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	return filepath.Join(dir, name)
}

func piWindowFor(key, cwd, sessionID string, notBefore time.Time) *piWindow {
	return &piWindow{
		key:       key,
		entry:     state.SessionMapEntry{SessionID: sessionID, CWD: cwd},
		windowID:  key,
		notBefore: notBefore,
	}
}

func TestPiFileCreatedTime(t *testing.T) {
	got := piFileCreatedTime("2026-09-30T22-34-44-810Z_01a0f474-b6c9-70cc-a256-108c9ecd06ba.jsonl")
	want := time.Date(2026, 9, 30, 22, 34, 44, 810_000_000, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("piFileCreatedTime = %v, want %v", got, want)
	}
	if !piFileCreatedTime("garbage.jsonl").IsZero() {
		t.Fatal("expected zero time for unparseable name")
	}
}

// The production incident (01/10): two pi panes share one cwd. The old
// newest-file rule bound the OLD pane to the NEW pane's transcript, so the
// same reply was relayed to every open topic. The old pane must keep its
// own file; the new pane gets the newest one.
func TestResolvePiBindings_TwoPanesSharedCWD(t *testing.T) {
	dir := t.TempDir()
	idA := "aaaaaaaa-0000-0000-0000-000000000001" // old pane's transcript
	idB := "bbbbbbbb-0000-0000-0000-000000000002" // new pane's transcript
	writePiSessionFile(t, dir, time.Date(2026, 9, 30, 22, 34, 44, 0, time.UTC), idA)
	writePiSessionFile(t, dir, time.Date(2026, 10, 1, 0, 56, 16, 0, time.UTC), idB)

	wOld := piWindowFor("maquinista:3", "/repo", idA, time.Date(2026, 9, 30, 22, 34, 30, 0, time.UTC))
	wNew := piWindowFor("maquinista:4", "/repo", "", time.Date(2026, 10, 1, 0, 56, 10, 0, time.UTC))

	inv := func(string) []piFile { return readPiInventory(dir) }
	bound := resolvePiBindings([]*piWindow{wOld, wNew}, inv)

	if bound["maquinista:3"].id != idA {
		t.Fatalf("old pane bound to %v, want its own %s", bound["maquinista:3"].id, idA)
	}
	if bound["maquinista:4"].id != idB {
		t.Fatalf("new pane bound to %v, want %s", bound["maquinista:4"].id, idB)
	}
}

// State as found in production after the misfire: both windows held the
// same transcript id. Resolution must re-pair deterministically — newest
// pane ↔ newest file — instead of leaving the theft in place.
func TestResolvePiBindings_DuplicateClaimsRepaired(t *testing.T) {
	dir := t.TempDir()
	idA := "aaaaaaaa-0000-0000-0000-000000000001"
	idB := "bbbbbbbb-0000-0000-0000-000000000002"
	writePiSessionFile(t, dir, time.Date(2026, 9, 30, 22, 34, 44, 0, time.UTC), idA)
	writePiSessionFile(t, dir, time.Date(2026, 10, 1, 0, 56, 16, 0, time.UTC), idB)

	wOld := piWindowFor("maquinista:3", "/repo", idB, time.Date(2026, 9, 30, 22, 34, 30, 0, time.UTC))
	wNew := piWindowFor("maquinista:4", "/repo", idB, time.Date(2026, 10, 1, 0, 56, 10, 0, time.UTC))

	inv := func(string) []piFile { return readPiInventory(dir) }
	bound := resolvePiBindings([]*piWindow{wOld, wNew}, inv)

	if bound["maquinista:4"].id != idB {
		t.Fatalf("new pane bound to %v, want %s", bound["maquinista:4"].id, idB)
	}
	if bound["maquinista:3"].id != idA {
		t.Fatalf("old pane bound to %v, want repaired to %s", bound["maquinista:3"].id, idA)
	}
}

// pi creates its transcript lazily (~26s after spawn). A fresh pane with no
// file of its own yet must stay unbound — never borrow a sibling's file.
func TestResolvePiBindings_LazyNewPaneStaysUnbound(t *testing.T) {
	dir := t.TempDir()
	idA := "aaaaaaaa-0000-0000-0000-000000000001"
	idB := "bbbbbbbb-0000-0000-0000-000000000002"
	writePiSessionFile(t, dir, time.Date(2026, 9, 30, 22, 34, 44, 0, time.UTC), idA)
	writePiSessionFile(t, dir, time.Date(2026, 10, 1, 0, 56, 16, 0, time.UTC), idB)

	wOld := piWindowFor("maquinista:3", "/repo", idA, time.Date(2026, 9, 30, 22, 34, 30, 0, time.UTC))
	wNew := piWindowFor("maquinista:4", "/repo", idB, time.Date(2026, 10, 1, 0, 56, 10, 0, time.UTC))
	wFresh := piWindowFor("maquinista:5", "/repo", "", time.Date(2026, 10, 1, 1, 30, 0, 0, time.UTC))

	inv := func(string) []piFile { return readPiInventory(dir) }
	bound := resolvePiBindings([]*piWindow{wOld, wNew, wFresh}, inv)

	if _, ok := bound["maquinista:5"]; ok {
		t.Fatalf("fresh pane stole %v before its own transcript exists", bound["maquinista:5"].id)
	}
	if bound["maquinista:3"].id != idA || bound["maquinista:4"].id != idB {
		t.Fatalf("siblings disturbed: %v / %v", bound["maquinista:3"].id, bound["maquinista:4"].id)
	}
}

// A binding carried over from a previous pane epoch points at a file older
// than the pane and must be invalidated (self-correcting the lazy-creation
// gap), while files claimed by other live windows stay off limits.
func TestResolvePiBindings_StaleEpochRebindsToOwnFile(t *testing.T) {
	dir := t.TempDir()
	idA := "aaaaaaaa-0000-0000-0000-000000000001" // dead epoch's transcript
	idB := "bbbbbbbb-0000-0000-0000-000000000002" // another live pane's
	idC := "cccccccc-0000-0000-0000-000000000003" // this pane's new transcript
	writePiSessionFile(t, dir, time.Date(2026, 9, 30, 22, 34, 44, 0, time.UTC), idA)
	writePiSessionFile(t, dir, time.Date(2026, 10, 1, 0, 56, 16, 0, time.UTC), idB)
	writePiSessionFile(t, dir, time.Date(2026, 10, 1, 2, 10, 26, 0, time.UTC), idC)

	wOther := piWindowFor("maquinista:4", "/repo", idB, time.Date(2026, 10, 1, 0, 56, 10, 0, time.UTC))
	wRestarted := piWindowFor("maquinista:3", "/repo", idA, time.Date(2026, 10, 1, 2, 10, 0, 0, time.UTC))

	inv := func(string) []piFile { return readPiInventory(dir) }
	bound := resolvePiBindings([]*piWindow{wOther, wRestarted}, inv)

	if bound["maquinista:3"].id != idC {
		t.Fatalf("restarted pane bound to %v, want its own new transcript %s", bound["maquinista:3"].id, idC)
	}
	if bound["maquinista:4"].id != idB {
		t.Fatalf("sibling bound to %v, want %s", bound["maquinista:4"].id, idB)
	}
}
