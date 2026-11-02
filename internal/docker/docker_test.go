package docker

import (
	"testing"
	"time"
)

func TestParseInspect(t *testing.T) {
	state, err := parseInspect([]byte(`[{
		"RestartCount": 2,
		"State": {
			"Running": true, "Restarting": true, "OOMKilled": true, "ExitCode": 137,
			"FinishedAt": "2026-11-02T21:33:27.290695876Z"
		},
		"NetworkSettings": {"Networks": {"shipit-apps": {"IPAddress": "172.20.0.5"}}}
	}]`))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 2, 21, 33, 27, 290695876, time.UTC)
	if !state.Running || !state.Restarting || !state.OOMKilled || state.RestartCount != 2 ||
		state.ExitCode != 137 || state.IP != "172.20.0.5" || !state.FinishedAt.Equal(want) {
		t.Errorf("state = %+v", state)
	}
}

func TestParseInspectRejectsUnexpectedOutput(t *testing.T) {
	for _, out := range []string{`nope`, `[]`} {
		if _, err := parseInspect([]byte(out)); err == nil {
			t.Errorf("%q: expected an error", out)
		}
	}
}
