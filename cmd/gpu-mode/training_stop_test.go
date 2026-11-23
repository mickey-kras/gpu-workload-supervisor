package main

import (
	"flag"
	"testing"
	"time"
)

func TestStopGraceFlag(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		command string
		ok      bool
	}{
		{nil, trainingStopCommand, true},
		{[]string{"-stop-grace", "30s"}, trainingStopCommand, true},
		{nil, "training-status", true},
		{[]string{"-stop-grace", "30s"}, "training-status", false},
		{[]string{"-stop-grace", "0s"}, trainingStopCommand, false},
	} {
		flags := flag.NewFlagSet("gpu-mode", flag.ContinueOnError)
		grace := flags.Duration("stop-grace", 2*time.Minute, "")
		if err := flags.Parse(tc.args); err != nil {
			t.Fatal(err)
		}
		if err := validateStopGrace(flags, tc.command, *grace); (err == nil) != tc.ok {
			t.Fatalf("%v %s: %v", tc.args, tc.command, err)
		}
	}
	if err := validateTrainingFlags(trainingStopCommand, "job-a", false); err != nil {
		t.Fatal(err)
	}
}
