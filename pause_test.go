package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// procStateOf reads the one-letter scheduler state from /proc: T is stopped.
func procStateOf(t *testing.T, pid int) string {
	t.Helper()
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	return strings.Fields(s[strings.LastIndexByte(s, ')')+1:])[0]
}

func sleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

func TestPauserFreezesAndThawsTheChild(t *testing.T) {
	withTempState(t)
	var p pauser

	cmd := sleeper(t)
	p.Register(cmd.Process)

	if !p.apply(true) {
		t.Fatal("first pause must report a change")
	}
	time.Sleep(50 * time.Millisecond)
	if s := procStateOf(t, cmd.Process.Pid); s != "T" {
		t.Fatalf("child should be stopped (T), is %q", s)
	}
	if p.apply(true) {
		t.Error("a repeated pause is not a change")
	}
	if !p.apply(false) {
		t.Error("resume must report a change")
	}
	time.Sleep(50 * time.Millisecond)
	if s := procStateOf(t, cmd.Process.Pid); s == "T" {
		t.Fatal("child should be running again")
	}

	// A child registered while a pause stands is frozen on arrival, so a
	// pause pressed during verify also holds the next encode.
	_ = p.apply(true)
	cmd2 := sleeper(t)
	p.Register(cmd2.Process)
	time.Sleep(50 * time.Millisecond)
	if s := procStateOf(t, cmd2.Process.Pid); s != "T" {
		t.Fatalf("a child started during a pause must be frozen, is %q", s)
	}
	_ = p.apply(false)
}

func TestPauseRequestIsAFile(t *testing.T) {
	withTempState(t)
	if PauseRequested() {
		t.Fatal("nothing requested yet")
	}
	if err := RequestPause(); err != nil {
		t.Fatal(err)
	}
	if !PauseRequested() {
		t.Fatal("the request must be visible")
	}
	if err := RequestResume(); err != nil {
		t.Fatal(err)
	}
	if PauseRequested() || RequestResume() != nil {
		t.Fatal("resume must clear the request and be idempotent")
	}
}
