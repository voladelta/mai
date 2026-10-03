package mai

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvalRunnersReportFailures(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq unavailable: %v", err)
	}

	var events bytes.Buffer
	for _, event := range []map[string]any{
		{"type": "model.completed"},
		{"type": "model.completed", "worker_id": "worker-1"},
		{"type": "model.failed"},
		{"type": "tool.started", "name": "bash"},
		{"type": "tool.completed", "name": "bash", "output": textToolOutput(`{"ok":true,"metadata":{"ok":false}}`)},
		{"type": "tool.started", "name": "bash", "worker_id": "worker-1"},
		{"type": "tool.completed", "name": "bash", "worker_id": "worker-1", "output": textToolOutput(`{"ok":false,"exit_code":1}`)},
		{"type": "tool.started", "name": "python"},
		{"type": "tool.completed", "name": "python", "output_omitted": true},
	} {
		if err := writeJSONLEvent(&events, event); err != nil {
			t.Fatal(err)
		}
	}

	for _, test := range []struct {
		name      string
		runner    string
		agentExit string
		gradeExit string
		wrongEdit string
		wantGrade string
		wantExit  int
	}{
		{
			name: "coding pass", runner: "run.sh",
			agentExit: "0", gradeExit: "0", wantGrade: "pass",
		},
		{
			name: "coding agent failed", runner: "run.sh",
			agentExit: "1", gradeExit: "0", wantGrade: "fail", wantExit: 1,
		},
		{
			name: "coding grader failed", runner: "run.sh",
			agentExit: "0", gradeExit: "1", wantGrade: "fail", wantExit: 1,
		},
		{
			name: "patch pass", runner: "patch-rate.sh",
			agentExit: "0", gradeExit: "0", wantGrade: "pass",
		},
		{
			name: "patch agent failed", runner: "patch-rate.sh",
			agentExit: "1", gradeExit: "0", wantGrade: "fail", wantExit: 1,
		},
		{
			name: "patch grader failed", runner: "patch-rate.sh",
			agentExit: "0", gradeExit: "1", wantGrade: "fail", wantExit: 1,
		},
		{
			name: "patch wrong occurrence", runner: "patch-rate.sh",
			agentExit: "0", gradeExit: "1", wrongEdit: "1",
			wantGrade: "wrong-edit", wantExit: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range map[string][]byte{
				"events.jsonl": events.Bytes(),
				"mai":          []byte("#!/bin/sh\ncat \"$MAI_TEST_EVENTS\"\nprintf '%s\\n' \"$*\" >> \"$MAI_TEST_ARGS\"\nexit \"$MAI_TEST_AGENT_EXIT\"\n"),
				"go":           []byte("#!/bin/sh\nif [ -n \"$MAI_TEST_WRONG_EDIT\" ]; then printf 'wrong edit: RetryTimeout = 45, want 30\\n'; fi\nexit \"$MAI_TEST_GRADE_EXIT\"\n"),
			} {
				if err := os.WriteFile(filepath.Join(dir, name), content, 0700); err != nil {
					t.Fatal(err)
				}
			}

			t.Setenv("TMPDIR", dir)
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("MAI_EVAL_BIN", filepath.Join(dir, "mai"))
			t.Setenv("MAI_EVAL_MODE", "")
			t.Setenv("MAI_EVAL_PROVIDER", "")
			t.Setenv("MAI_TEST_EVENTS", filepath.Join(dir, "events.jsonl"))
			t.Setenv("MAI_TEST_ARGS", filepath.Join(dir, "args.txt"))
			t.Setenv("MAI_TEST_AGENT_EXIT", test.agentExit)
			t.Setenv("MAI_TEST_GRADE_EXIT", test.gradeExit)
			t.Setenv("MAI_TEST_WRONG_EDIT", test.wrongEdit)

			args := []string{filepath.Join("..", "..", "evals", test.runner)}
			wantRows := 12
			if test.runner == "run.sh" {
				args = append(args, "startup-timeout")
				wantRows = 1
			}

			cmd := exec.Command("sh", args...)
			output, err := cmd.CombinedOutput()
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != test.wantExit {
				t.Fatalf("runner exit: %v, want %d\n%s", err, test.wantExit, output)
			}

			rows := 0
			for _, line := range strings.Split(string(output), "\n") {
				fields := strings.Split(line, "\t")
				if len(fields) != 6 || fields[1] == "grade" {
					continue
				}
				rows++
				if fields[1] != test.wantGrade || strings.Join(fields[3:], "\t") != "3\t3\t1" {
					t.Fatalf("result row = %q, want grade %s and counts 3/3/1", line, test.wantGrade)
				}
			}
			if rows != wantRows {
				t.Fatalf("got %d result rows, want %d\n%s", rows, wantRows, output)
			}

			invocations, err := os.ReadFile(filepath.Join(dir, "args.txt"))
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(invocations)), "\n") {
				if !strings.HasSuffix(line, "--provider deepseek --jsonl --no-input --skip-skills") {
					t.Fatalf("runner does not use the current default model and isolated CLI flags: %q", line)
				}
			}
		})
	}
}

func TestEvalRunnerResumesCorrectionAndReportsItsFailure(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq unavailable")
	}
	for _, failFollowup := range []bool{false, true} {
		t.Run(map[bool]string{false: "pass", true: "followup failure"}[failFollowup], func(t *testing.T) {
			dir := t.TempDir()
			mai := filepath.Join(dir, "mai")
			body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$MAI_TEST_ARGS\"\ncase \" $* \" in *' --last '*) exit \"$MAI_TEST_FOLLOWUP_EXIT\" ;; esac\nexit 0\n"
			if err := os.WriteFile(mai, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "go"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TMPDIR", dir)
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("MAI_EVAL_BIN", mai)
			t.Setenv("MAI_EVAL_MODE", "flash")
			t.Setenv("MAI_EVAL_PROVIDER", "enclave")
			t.Setenv("MAI_EVAL_CONFIG", "")
			argsPath := filepath.Join(dir, "args.txt")
			t.Setenv("MAI_TEST_ARGS", argsPath)
			wantExit := 0
			wantGrade := "pass"
			followupExit := "0"
			if failFollowup {
				wantExit = 1
				wantGrade = "fail"
				followupExit = "7"
			}
			t.Setenv("MAI_TEST_FOLLOWUP_EXIT", followupExit)
			cmd := exec.Command("sh", filepath.Join("..", "..", "evals", "run.sh"), "twitter-thread")
			output, err := cmd.CombinedOutput()
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != wantExit || !strings.Contains(string(output), "twitter-thread\t"+wantGrade+"\t") {
				t.Fatalf("followup grade: %v %s", err, output)
			}
			data, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			// Prompts are multiline, so inspect option suffixes rather than
			// assuming each invocation is one line in this diagnostic file.
			first, last := false, false
			for _, line := range lines {
				first = first || strings.HasSuffix(line, "--provider enclave --f --persist --jsonl --no-input --skip-skills")
				last = last || strings.HasSuffix(line, "--last --provider enclave --jsonl --no-input --skip-skills")
			}
			if !first || !last {
				t.Fatalf("initial/resumed CLI flags: %s", data)
			}
		})
	}
}
