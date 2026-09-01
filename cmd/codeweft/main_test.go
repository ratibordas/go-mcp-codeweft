package main

import (
	"context"
	"strings"
	"testing"
)

func TestRunHandlesHelpAndRejectsUnwiredCommands(t *testing.T) {
	if err := run(context.Background(), []string{"help"}); err != nil {
		t.Fatalf("help returned %v", err)
	}
	err := run(context.Background(), []string{"serve"})
	if err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Fatalf("unwired command error = %v", err)
	}
}
