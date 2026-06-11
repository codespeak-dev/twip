package main

import (
	"runtime/debug"

	"github.com/spf13/cobra"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print twip version / build info",
		RunE: func(cmd *cobra.Command, _ []string) error {
			bi, ok := debug.ReadBuildInfo()
			if !ok {
				cmd.Println("twip (unknown build)")
				return nil
			}
			ver := bi.Main.Version
			if ver == "" {
				ver = "(devel)"
			}
			var rev, when, modified string
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					rev = s.Value
				case "vcs.time":
					when = s.Value
				case "vcs.modified":
					modified = s.Value
				}
			}
			cmd.Printf("twip %s\n", ver)
			if rev != "" {
				short := rev
				if len(short) > 12 {
					short = short[:12]
				}
				dirty := ""
				if modified == "true" {
					dirty = " (dirty)"
				}
				cmd.Printf("  commit %s%s  %s\n", short, dirty, when)
			}
			cmd.Printf("  built with %s\n", bi.GoVersion)
			return nil
		},
	}
}
