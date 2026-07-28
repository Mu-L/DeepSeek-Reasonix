package repair

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func prepareTestFileUpdateClaim(t *testing.T) (*UpdateTransaction, string, []string) {
	t.Helper()
	t.Setenv("REASONIX_HOME", t.TempDir())
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{
		filepath.Join(dir, "reasonix-desktop"),
		filepath.Join(dir, "reasonix-guard"),
		filepath.Join(dir, "reasonix"),
	}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := PrepareFileUpdate("v1", "v2", paths[0], paths[1:]...)
	if err != nil {
		t.Fatal(err)
	}
	return tx, paths[0], paths
}

func TestClaimPendingFileUpdateRejectsReleaseUnitMismatch(t *testing.T) {
	tx, launcher, paths := prepareTestFileUpdateClaim(t)
	_, release, err := ClaimPendingFileUpdate(
		tx.ToVersion,
		tx.CreatedAt,
		launcher,
		paths[:len(paths)-1],
		time.Second,
	)
	if release != nil {
		release()
	}
	if err == nil || !strings.Contains(err.Error(), "release unit does not match") {
		t.Fatalf("claim error = %v, want release-unit mismatch", err)
	}
}

func TestClaimPendingFileUpdateRejectsReplacementWhileLocking(t *testing.T) {
	tx, launcher, paths := prepareTestFileUpdateClaim(t)
	holder, err := LockRepairMutations(paths...)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	originalBeforeLock := repairMutationBeforeLock
	repairMutationBeforeLock = func([]string) {
		entered <- struct{}{}
	}
	t.Cleanup(func() { repairMutationBeforeLock = originalBeforeLock })

	done := make(chan error, 1)
	go func() {
		_, release, err := ClaimPendingFileUpdate(
			tx.ToVersion,
			tx.CreatedAt,
			launcher,
			paths,
			2*time.Second,
		)
		if release != nil {
			release()
		}
		done <- err
	}()

	select {
	case <-entered:
	case err := <-done:
		holder()
		t.Fatalf("claim failed before taking release-unit locks: %v", err)
	}
	replacement := *tx
	replacement.CreatedAt = time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano)
	if err := WritePendingUpdate(&replacement); err != nil {
		holder()
		t.Fatal(err)
	}
	holder()

	err = <-done
	if err == nil || !strings.Contains(err.Error(), "changed while waiting") {
		t.Fatalf("claim error = %v, want replacement rejection", err)
	}
}

func TestClaimPendingFileUpdateHoldsCompleteReleaseUnit(t *testing.T) {
	tx, launcher, paths := prepareTestFileUpdateClaim(t)
	_, release, err := ClaimPendingFileUpdate(
		tx.ToVersion,
		tx.CreatedAt,
		launcher,
		paths,
		time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}

	waiter := make(chan error, 1)
	go func() {
		unlock, err := LockRepairMutationsTimeout(time.Second, paths...)
		if err == nil {
			unlock()
		}
		waiter <- err
	}()
	select {
	case err := <-waiter:
		release()
		t.Fatalf("release-unit lock escaped the claim: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	if err := <-waiter; err != nil {
		t.Fatalf("release-unit lock after claim: %v", err)
	}
}
