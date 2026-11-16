package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	romcompressor "github.com/olsonb97/romcompressor.go"
)

func executePlan(ctx context.Context, a arguments, stdout io.Writer) error {
	plan, err := romcompressor.PlanConversions(ctx, a.paths[0], options(a))
	if err != nil {
		return err
	}
	if a.json {
		return json.NewEncoder(stdout).Encode(plan)
	}
	fmt.Fprintln(stdout, "TARGET          MEDIA  OUTPUT     DEPENDENCIES")
	for _, target := range plan.Targets {
		label := string(target.Format)
		if target.ExtractAll {
			label += " (--all)"
		}
		kind := "file"
		if target.Directory {
			kind = "directory"
		} else if plan.Playlist {
			kind = "playlist"
		}
		dependencies := make([]string, 0, len(target.Dependencies))
		for _, dependency := range target.Dependencies {
			dependencies = append(dependencies, capabilityStatus(dependency))
		}
		fmt.Fprintf(stdout, "%-15s %-6s %-10s %s\n", label, target.Media, kind, strings.Join(dependencies, ", "))
		if target.NeedsArchiveListing {
			fmt.Fprintln(stdout, "  Use inspect to list archive entries before selecting --entry.")
		} else if target.RequiresEntry {
			fmt.Fprintln(stdout, "  Select a file with --entry.")
		}
	}
	if len(plan.Targets) == 0 {
		fmt.Fprintln(stdout, "No supported conversions for this input.")
	}
	_, err = fmt.Fprintln(stdout, "\nPreflight does not verify the full payload, engine versions, keys, or emulator compatibility.")
	return err
}
