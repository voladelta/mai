package mai

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRMInsideRepositoryDoesNotNeedApproval(t *testing.T) {
	root := t.TempDir()
	if required, reason := requiresRMApproval("rm -rf build ./tmp", root, root); required {
		t.Fatalf("unexpected approval: %s", reason)
	}
}

func TestRMOutsideRepositoryNeedsApproval(t *testing.T) {
	root := t.TempDir()
	if required, _ := requiresRMApproval("rm -f /tmp/mai-outside", root, root); !required {
		t.Fatal("expected approval")
	}
	if required, _ := requiresRMApproval("rm ../outside", root, root); !required {
		t.Fatal("expected approval for parent path")
	}
}

func TestDynamicRMTargetNeedsApproval(t *testing.T) {
	root := t.TempDir()
	for _, command := range []string{"rm -rf $TARGET", "rm *.tmp", "cd /tmp && rm thing", "sudo -n rm /tmp/thing"} {
		if required, _ := requiresRMApproval(command, root, root); !required {
			t.Fatalf("expected approval for %q", command)
		}
	}
}

func TestMentioningRMIsNotACommand(t *testing.T) {
	root := t.TempDir()
	if required, reason := requiresRMApproval(`printf '%s\n' rm`, root, root); required {
		t.Fatalf("unexpected approval: %s", reason)
	}
}

func TestWrappedRMInsideRepositoryDoesNotNeedApproval(t *testing.T) {
	root := t.TempDir()
	commands := []string{
		"sudo -n rm -rf build",
		"env MODE=test command rm -- ./tmp",
		"! rm -f ./missing",
		"time rm -f ./missing",
		"exec rm -f ./missing",
		"printf done && rm 'quoted path'",
	}
	for _, command := range commands {
		if required, reason := requiresRMApproval(command, root, root); required {
			t.Fatalf("%q unexpectedly needs approval: %s", command, reason)
		}
	}
}

func TestShellPrefixesDoNotBypassExternalRMApproval(t *testing.T) {
	root := t.TempDir()
	for _, command := range []string{
		"! rm -rf /tmp/mai-outside",
		"time rm -rf /tmp/mai-outside",
		"exec rm -rf /tmp/mai-outside",
		"exec -a remove rm -rf /tmp/mai-outside",
		"exec -a X=y rm -rf /tmp/mai-outside",
		"sudo -u root rm -rf /tmp/mai-outside",
		"env -u TARGET rm -rf /tmp/mai-outside",
	} {
		if required, reason := requiresRMApproval(command, root, root); !required {
			t.Errorf("%q did not require approval; reason=%q", command, reason)
		}
	}
}

func TestUnclassifiedRMExecutionNeedsApproval(t *testing.T) {
	root := t.TempDir()
	for _, command := range []string{
		"nice -n 1 rm -rf /tmp/mai-outside",
		"timeout 5 rm -rf /tmp/mai-outside",
		"sh -c 'rm -rf /tmp/mai-outside'",
	} {
		if required, reason := requiresRMApproval(command, root, root); !required {
			t.Errorf("%q did not require approval; reason=%q", command, reason)
		}
	}
}

func TestRMSymlinkOutsideRepositoryNeedsApproval(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "outside-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if required, _ := requiresRMApproval("rm outside-link", root, root); !required {
		t.Fatal("expected approval for a symlink that resolves outside the repository")
	}
}
