package pkg

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

var Version = "dev"

var VersionCmd = &cobra.Command{
	Use:               "version",
	Short:             "Print version information",
	DisableAutoGenTag: true,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("cyclone %s compiled with %v on %v/%v\n",
			Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	},
}

func init() {
	RootCmd.AddCommand(VersionCmd)
}
