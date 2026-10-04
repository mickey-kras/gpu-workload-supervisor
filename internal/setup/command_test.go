package setup

import (
	"context"
	"testing"
)

func TestBoundedCommandOutput(t *testing.T) {
	data, err := boundedCommand(context.Background(), "/usr/bin/printf", "hello")
	if err != nil || string(data) != "hello" {
		t.Fatalf("%s %v", data, err)
	}
	data, err = boundedCommand(context.Background(), "/usr/bin/head", "-c", "1048577", "/dev/zero")
	if err == nil || len(data) != commandOutputLimit {
		t.Fatalf("len %d err %v", len(data), err)
	}
}
