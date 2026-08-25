package mai

import "testing"

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
