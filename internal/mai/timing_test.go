package mai

import (
	"os/exec"
	"strings"
	"testing"
)

func TestTimingReportSumsDurations(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq unavailable: %v", err)
	}

	for _, test := range []struct {
		name   string
		events string
		want   string
	}{
		{
			name: "ordinary model and tool durations",
			events: `{"type":"model.completed","duration_ms":100}
{"type":"model.failed","duration_ms":25}
{"type":"tool.completed","name":"bash","duration_ms":200}
{"type":"task.completed","duration_ms":400}
`,
			want: "/dev/stdin\t400\t125\t200\t75\n",
		},
		{
			name: "multiple model and tool durations",
			events: `{"type":"model.completed","duration_ms":100}
{"type":"model.completed","duration_ms":600}
{"type":"tool.completed","name":"bash","duration_ms":100}
{"type":"model.completed","duration_ms":100}
{"type":"tool.completed","name":"edit","duration_ms":20}
{"type":"model.completed","duration_ms":100}
{"type":"task.completed","duration_ms":1020}
`,
			want: "/dev/stdin\t1020\t900\t120\t0\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd := exec.Command("sh", "../../evals/timing.sh", "/dev/stdin")
			cmd.Stdin = strings.NewReader(test.events)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("timing report: %v\n%s", err, output)
			}

			want := "events\ttask_ms\tmodel_ms\ttool_ms\tother_ms\n" + test.want
			if string(output) != want {
				t.Fatalf("timing report = %q, want %q", output, want)
			}
		})
	}
}
