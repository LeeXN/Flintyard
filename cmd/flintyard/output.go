package main

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"text/tabwriter"

	fy "github.com/LeeXN/Flintyard/pkg/flintyard"
)

func writeText(out io.Writer, data any) error {
	if logs, ok := data.(fy.LogsResult); ok {
		_, err := io.WriteString(out, logs.Content)
		return err
	}

	var text strings.Builder
	switch r := data.(type) {
	case nil:
		return nil
	case versionResult:
		fmt.Fprintf(&text, "Flintyard %s\nGo: %s\nFirecracker: %s\nProtocol: %s\n", r.Version, r.Go, r.Firecracker, r.Protocol)
	case fy.DoctorReport:
		if r.Mode == "" {
			break
		}
		status := "ready"
		if !r.Ready {
			status = "needs attention"
		}
		fmt.Fprintf(&text, "Host checks (%s, %s): %s\n", r.Mode, r.Architecture, status)
		for _, check := range r.Checks {
			fmt.Fprintf(&text, "  [%s] %s: %s\n", checkStatus(check.OK), check.Name, check.Detail)
		}
		for _, warning := range r.Warnings {
			fmt.Fprintf(&text, "  Warning: %s\n", warning)
		}
	case fy.ImageResult:
		imageText(&text, r)
	case fy.BuildResult:
		imageText(&text, r.ImageResult)
		if r.BuildID != "" {
			fmt.Fprintf(&text, "Build: %s\n", r.BuildID)
		}
		if r.ID != "" {
			fmt.Fprintf(&text, "Cache hit: %t\n", r.CacheHit)
		}
		if r.LogPath != "" {
			fmt.Fprintf(&text, "Build log: %s\n", r.LogPath)
		}
	case fy.ValidationResult:
		if r.Validation == "" {
			break
		}
		fmt.Fprintf(&text, "Image validation: %s\n", r.Validation)
		if r.ImageID != "" {
			fmt.Fprintf(&text, "Image ID: %s\n", r.ImageID)
		}
		fmt.Fprintf(&text, "  Static checks: %s\n  Boot: %s\n  Access: %s\n",
			validationStatus(r.StaticPassed), validationStatus(r.BootPassed), validationStatus(r.AccessPassed))
		if r.Retained {
			fmt.Fprintf(&text, "Retained environment: %s (%s)\n", r.EnvironmentName, r.EnvironmentID)
		}
		if r.Cleanup != nil {
			cleanupText(&text, *r.Cleanup)
		}
		if r.DiagnosticsPath != "" {
			fmt.Fprintf(&text, "Diagnostics: %s\n", r.DiagnosticsPath)
		}
		if r.RecordPath != "" {
			fmt.Fprintf(&text, "Validation record: %s\n", r.RecordPath)
		}
	case fy.EnvironmentResult:
		environmentText(&text, r)
	case fy.ListResult:
		if len(r.Environments) == 0 {
			fmt.Fprintln(&text, "No environments.")
			break
		}
		table := tabwriter.NewWriter(&text, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "NAME\tSTATE\tARCHITECTURE\tNODES")
		for _, env := range r.Environments {
			fmt.Fprintf(table, "%s\t%s\t%s\t%d\n", env.Name, env.State, env.Architecture, len(env.Nodes))
		}
		table.Flush()
	case fy.CleanupResult:
		cleanupText(&text, r)
	case fy.GCResult:
		gcText(&text, r)
	default:
		return fmt.Errorf("no text output for %T", data)
	}
	_, err := io.WriteString(out, text.String())
	return err
}

func checkStatus(passed bool) string {
	if passed {
		return "OK"
	}
	return "FAIL"
}

func validationStatus(passed bool) string {
	if passed {
		return "passed"
	}
	return "unconfirmed"
}

func imageText(out io.Writer, r fy.ImageResult) {
	if r.ID == "" {
		return
	}
	m := r.Manifest
	fmt.Fprintf(out, "Image: %s %s (%s)\nImage ID: %s\nOS: %s %s\nKernel: %s (%s)\n",
		m.Name, m.Version, m.Architecture, r.ID, m.OS.Family, m.OS.Release, m.Kernel.Release, m.Kernel.Origin)
	if r.PackageSHA256 != "" {
		fmt.Fprintf(out, "Package SHA-256: %s\n", r.PackageSHA256)
	}
	fmt.Fprintf(out, "Source trust: %s\nValidation: %s\n", r.SourceTrust, r.Validation)
	if r.KernelIssue != "" {
		fmt.Fprintf(out, "Kernel issue: %s\n", r.KernelIssue)
	}
}

func environmentText(out io.Writer, r fy.EnvironmentResult) {
	if r.EnvironmentID == "" {
		return
	}
	fmt.Fprintf(out, "Environment %s: %s (%s)\nID: %s\n", r.Name, r.State, r.Architecture, r.EnvironmentID)
	if len(r.Nodes) > 0 {
		table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "NODE\tSTATE\tIPV4\tACCESS\tPROBE")
		for _, node := range r.Nodes {
			access, probe := "-", "-"
			if node.Access.Host != "" && node.Access.Port > 0 {
				access = net.JoinHostPort(node.Access.Host, strconv.Itoa(node.Access.Port))
				if node.Access.Username != "" {
					access = node.Access.Username + "@" + access
				}
			}
			if node.LastProbeAt != nil {
				probe = "failed"
				if node.LastProbePassed {
					probe = "passed"
				}
				if node.Stale {
					probe = "last " + probe
				}
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", node.Name, node.State, node.IPv4, access, probe)
		}
		table.Flush()
	}
	for _, node := range r.Nodes {
		if node.ObservationError != nil {
			fmt.Fprintf(out, "  %s: %s\n", node.Name, node.ObservationError.Message)
		}
	}
	if network := r.Network; network != nil {
		for _, f := range network.Forwards {
			fmt.Fprintf(out, "  Forward: %s %s -> %s:%d\n", f.Protocol, f.HostEndpoint, f.Node, f.GuestPort)
		}
		for _, diagnostic := range network.Diagnostics {
			fmt.Fprintf(out, "  Network: %s\n", diagnostic)
		}
	}
	for _, warning := range r.Warnings {
		fmt.Fprintf(out, "  Warning: %s\n", warning)
	}
}

func cleanupText(out io.Writer, r fy.CleanupResult) {
	if r.AlreadyAbsent {
		fmt.Fprintf(out, "Environment %s is already absent.\n", r.Name)
	} else if r.EnvironmentID != "" {
		fmt.Fprintf(out, "Environment %s: %s\n", r.Name, r.State)
	}
	for _, err := range r.Errors {
		fmt.Fprintf(out, "  Cleanup error: %s\n", err)
	}
}

func gcText(out io.Writer, r fy.GCResult) {
	if r.Plan != nil {
		p := r.Plan
		fmt.Fprintf(out, "Cleanup plan: %d environments, %d builds, %d bootstraps\n", len(p.Targets), len(p.Builds), len(p.Bootstraps))
		for _, target := range p.Targets {
			fmt.Fprintf(out, "  Environment: %s (%s)\n", target.Name, target.EnvironmentID)
		}
		for _, target := range p.Builds {
			fmt.Fprintf(out, "  Build: %s\n", target.BuildID)
		}
		for _, target := range p.Bootstraps {
			fmt.Fprintf(out, "  Bootstrap: %s (%s)\n", target.Name, target.Scope)
		}
	}
	if r.Applied {
		fmt.Fprintf(out, "Cleaned: %d environments, %d builds, %d bootstraps\n", len(r.Cleaned), len(r.BuildsCleaned), len(r.BootstrapsCleaned))
	}
	for _, item := range r.Excluded {
		fmt.Fprintf(out, "  Skipped environment %s: %s\n", item.Name, item.Reason)
	}
	for _, item := range r.BuildsExcluded {
		fmt.Fprintf(out, "  Skipped build %s: %s\n", item.BuildID, item.Reason)
	}
	for _, item := range r.BootstrapsExcluded {
		fmt.Fprintf(out, "  Skipped bootstrap %s: %s\n", item.Path, item.Reason)
	}
}
