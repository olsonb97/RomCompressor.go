package main

import (
	"slices"
	"strings"
	"testing"
)

// Check every subset of the eleven public non-help options for each command.
// Format-specific restrictions are exercised by the conversion and CLI tests.
func TestCommandOptionCombinations(t *testing.T) {
	flags := []string{"format=zip", "output=result", "entry=track", "all", "media=cd", "keys=copy.keys",
		"temp-dir=scratch", "max-output=2GiB", "tool=7z=program", "dry-run", "json"}
	for command, permitted := range map[string]string{
		"compress": "format output media keys max-output tool json", "decompress": "output entry all media keys max-output tool json",
		"trim": "output dry-run json", "verify": "entry media keys temp-dir max-output tool json", "inspect": "tool json",
		"plan": "max-output tool json", "formats": "tool json", "batch": "tool keys json", "version": "json",
	} {
		t.Run(command, func(t *testing.T) {
			allowed := strings.Fields(permitted)
			for mask := 0; mask < 1<<len(flags); mask++ {
				args := []string{command}
				if command != "formats" && command != "version" {
					args = append(args, "source")
				}
				set := make(map[string]bool)
				want := true
				for i, flag := range flags {
					if mask&(1<<i) == 0 {
						continue
					}
					name, _, _ := strings.Cut(flag, "=")
					args = append(args, "--"+flag)
					set[name] = true
					want = want && slices.Contains(allowed, name)
				}
				if command == "compress" && !set["format"] || set["dry-run"] && set["output"] ||
					set["all"] && (!set["output"] || set["entry"] || set["media"] || set["keys"]) {
					want = false
				}
				a, err := parse(args)
				if want != (err == nil) || a.json != set["json"] {
					t.Fatalf("%v: accepted=%t json=%t, want accepted=%t json=%t (%v)", args, err == nil, a.json, want, set["json"], err)
				}
			}
		})
	}
}
