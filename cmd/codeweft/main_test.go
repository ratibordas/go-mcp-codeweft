package main

import (
	"context"
	"strings"
	"testing"
)

func TestRunHandlesHelpAndRejectsUnknownCommands(t *testing.T) {
	if err := run(context.Background(), []string{"help"}); err != nil {
		t.Fatalf("help returned %v", err)
	}
	err := run(context.Background(), []string{"unknown"})
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("unknown command error = %v", err)
	}
}

func TestRunValidatesBeforeOpeningExternalServices(t *testing.T) {
	for _, args := range [][]string{{"serve"}, {"search", "--project", "/tmp"}, {"purge", "--project", "/tmp"}} {
		err := run(context.Background(), args)
		if err == nil {
			t.Fatalf("%v unexpectedly succeeded", args)
		}
	}
	err := run(context.Background(), []string{"benchmark", "--project", "/tmp", "--suite", "/tmp/codeweft-missing-suite.json"})
	if err == nil || !strings.Contains(err.Error(), "benchmark suite") {
		t.Fatalf("benchmark validation error = %v", err)
	}
}
