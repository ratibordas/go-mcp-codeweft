package main

import (
	"context"
	"fmt"
	"log"
	"os"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		log.Print(err)
	}
}

func run(_ context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" {
		return nil
	}
	return fmt.Errorf("command %q is not wired yet", args[0])
}
