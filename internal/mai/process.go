package mai

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ownedCommand binds a process group to this Go process without a registration
// race. The shell starts its watcher before exec. Only Go has the pipe writer;
// the command has neither pipe end, and the watcher has no output descriptors.
func ownedCommand(ctx context.Context, executable string, args ...string) (*exec.Cmd, func(), error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	var cmd *exec.Cmd
	cleanup := func() {
		if cmd != nil && cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = writer.Close()
		_ = reader.Close()
	}
	const wrapper = `(while IFS= read -r line <&3; do :; done; kill -KILL -- "-$$") </dev/null >/dev/null 2>&1 &
exec "$@" 3<&-`
	arguments := append([]string{"-c", wrapper, "mai-owned", executable}, args...)
	cmd = exec.CommandContext(ctx, "/bin/bash", arguments...)
	cmd.ExtraFiles = []*os.File{reader}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd, cleanup, nil
}
