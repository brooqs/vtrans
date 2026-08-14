package main

import "testing"

func TestLastOverwrite(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "Done. 39 processed", "Done. 39 processed"},
		{"colour only", "\x1b[36mTotal saved: 44.2 GB\x1b[0m", "Total saved: 44.2 GB"},
		{
			// The shape the encoder actually writes: repeated redraws of one line,
			// each preceded by \r and an erase-line escape, ending in the result.
			"progress redraws",
			"\r\x1b[K   \x1b[2m1.9%  613 fps\x1b[0m" +
				"\r\x1b[K   \x1b[2m50.1%  650 fps\x1b[0m" +
				"\r\x1b[K   \x1b[32m313 MB -> 112 MB (201 MB saved, 1m19s) replaced\x1b[0m",
			"313 MB -> 112 MB (201 MB saved, 1m19s) replaced",
		},
		{"erase-line escape is stripped", "\x1b[K   hevc 1920x1080 -> AV1", "hevc 1920x1080 -> AV1"},
	}
	for _, c := range cases {
		if got := lastOverwrite(c.in); got != c.want {
			t.Errorf("%s: lastOverwrite() = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSplitStamp(t *testing.T) {
	l := splitStamp("2026-08-14T10:14:19+00:00 ai-server vtrans[2964303]: Done. 39 processed")
	if l.At != "2026-08-14T10:14:19+00:00" {
		t.Errorf("At = %q", l.At)
	}
	if l.Text != "Done. 39 processed" {
		t.Errorf("Text = %q", l.Text)
	}

	// systemd's own lines carry no [pid] and must survive the same path.
	l = splitStamp("2026-08-14T10:14:19+00:00 ai-server systemd[1]: vtrans.service: Deactivated successfully.")
	if l.Text != "vtrans.service: Deactivated successfully." {
		t.Errorf("systemd line Text = %q", l.Text)
	}
}

func TestServiceStateWaiting(t *testing.T) {
	// The state that reads as "it stopped" but is really the gap between runs.
	if !(ServiceState{Active: "activating", Sub: "auto-restart"}).Waiting() {
		t.Error("activating/auto-restart must count as waiting")
	}
	if (ServiceState{Active: "active", Sub: "running"}).Waiting() {
		t.Error("a running service is not waiting")
	}
	if (ServiceState{Active: "inactive", Sub: "dead"}).Waiting() {
		t.Error("a stopped service is not waiting")
	}
}
