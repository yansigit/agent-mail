// Command mailmux is the multi-provider build of olk. See docs/mailmux/.
package main

import (
	"fmt"
	"os"

	"github.com/rlrghb/olkcli/internal/cmd"
)

func main() {
	if err := isolateEnv(os.Environ(), os.Setenv, os.Unsetenv); err != nil {
		fmt.Fprintf(os.Stderr, "mailmux: %v\n", err)
		os.Exit(1)
	}
	configure()
	os.Exit(cmd.Execute())
}
