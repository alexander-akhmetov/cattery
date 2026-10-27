package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/alexander-akhmetov/cattery/internal/config"
)

func runConfig(out, diagnostics io.Writer, args []string) int {
	if len(args) != 2 || args[0] != "resume-prefix" {
		fmt.Fprintln(diagnostics, "usage: cattery config resume-prefix <claude|codex|opencode|pi>")
		return 2
	}
	switch args[1] {
	case "claude", "codex", "opencode", "pi":
	default:
		fmt.Fprintf(diagnostics, "cattery config: unknown agent %q\n", args[1])
		return 2
	}
	settings, err := config.Load()
	if err != nil {
		fmt.Fprintln(diagnostics, "cattery config:", err)
		return 1
	}
	if err := json.NewEncoder(out).Encode(settings.ResumePrefix(args[1])); err != nil {
		fmt.Fprintln(diagnostics, "cattery config:", err)
		return 1
	}
	return 0
}
