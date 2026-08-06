package main

import (
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
)

func TestRoute(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		want     string
		wantArgs []string
		wantErr  bool
	}{
		// The two forms that existed before subcommands.
		{name: "no arguments opens the picker", args: nil, want: cmdPicker},
		{name: "empty argument list", args: []string{}, want: cmdPicker},
		{name: "print", args: []string{"-print"}, want: cmdPrint},
		{name: "print, long form", args: []string{"--print"}, want: cmdPrint},
		{name: "print=false is still the picker", args: []string{"-print=false"}, want: cmdPicker},

		{name: "state working", args: []string{"state", "working"}, want: cmdState, wantArgs: []string{"working"}},
		{name: "state blocked", args: []string{"state", "blocked"}, want: cmdState, wantArgs: []string{"blocked"}},
		{name: "state idle", args: []string{"state", "idle"}, want: cmdState, wantArgs: []string{"idle"}},
		{name: "state clear", args: []string{"state", "clear"}, want: cmdState, wantArgs: []string{"clear"}},
		// An unknown word is the state writer's to ignore, not routing's.
		{name: "state with an unknown word", args: []string{"state", "nonsense"}, want: cmdState, wantArgs: []string{"nonsense"}},
		{name: "state with no word", args: []string{"state"}, want: cmdState},

		{name: "setup", args: []string{"setup"}, want: cmdSetup},
		{
			name:     "setup keeps its own flags",
			args:     []string{"setup", "--dry-run", "--kitty-dir", "/tmp/k"},
			want:     cmdSetup,
			wantArgs: []string{"--dry-run", "--kitty-dir", "/tmp/k"},
		},

		{name: "unknown subcommand", args: []string{"install"}, wantErr: true},
		{name: "unknown flag", args: []string{"-nope"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := route(tc.args, io.Discard)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("route(%v): got %+v, want an error", tc.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("route(%v): %v", tc.args, err)
			}
			if got.name != tc.want {
				t.Fatalf("route(%v): got %q, want %q", tc.args, got.name, tc.want)
			}
			if len(got.args) != len(tc.wantArgs) {
				t.Fatalf("route(%v) args: got %v, want %v", tc.args, got.args, tc.wantArgs)
			}
			for i := range tc.wantArgs {
				if got.args[i] != tc.wantArgs[i] {
					t.Fatalf("route(%v) arg %d: got %q, want %q", tc.args, i, got.args[i], tc.wantArgs[i])
				}
			}
		})
	}
}

// -h is not a routing failure: it asked for the usage text and got it.
func TestRouteHelp(t *testing.T) {
	var out strings.Builder
	if _, err := route([]string{"-h"}, &out); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("route(-h): got %v, want flag.ErrHelp", err)
	}
	if !strings.Contains(out.String(), "-print") {
		t.Fatalf("usage does not mention -print:\n%s", out.String())
	}
}
