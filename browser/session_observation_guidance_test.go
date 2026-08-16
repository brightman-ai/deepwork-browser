package browser

import (
	"errors"
	"strings"
	"testing"
)

func TestMissingSessionRefGuidesObserveNotRemovedSnapCommand(t *testing.T) {
	engine := newActionEngine(newSnapshotEngine())
	_, err := engine.resolveSemanticSelectorWithSession("@r1", true)
	if !errors.Is(err, ErrRefNotFound) {
		t.Fatalf("error = %v, want ErrRefNotFound", err)
	}
	removedGuidance := "run " + "snap first"
	if strings.Contains(err.Error(), removedGuidance) {
		t.Fatalf("guidance mentions removed snap command: %v", err)
	}
	if !strings.Contains(err.Error(), "run observe --id <id> first") {
		t.Fatalf("guidance = %v, want observe command", err)
	}
}
