package hostspawn

import (
	"os"
	"os/exec"
	"testing"
)

func TestReviewDarwinOriginalParentGeneration(t *testing.T) {
	parent := reviewParentVersion(os.Getpid())
	cmd := exec.Command("sleep", "20")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	self, original := reviewVersions(cmd.Process.Pid)
	if parent <= 0 || self <= parent || original != parent || !reviewOlderParent(cmd.Process.Pid, self) {
		t.Fatalf("parent %d, child %d, original parent %d", parent, self, original)
	}
}
