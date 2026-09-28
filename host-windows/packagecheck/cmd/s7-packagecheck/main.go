// s7-packagecheck reads a staged Windows payload. It never installs or runs it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"s7.local/packagecheck/audit"
	"s7.local/packagecheck/platform"
)

func main() {
	root := flag.String("package", "", "directory containing package.json")
	component := flag.String("component", "all", "all, monitor or camera")
	static := flag.Bool("static", false, "content-only development check; NEVER verifies trust")
	arch := flag.String("arch", "", "simulated x64/arm64, allowed only with --static")
	build := flag.Uint("build", 0, "simulated OS build, allowed only with --static")
	flag.Parse()
	if *root == "" || flag.NArg() != 0 || (!*static && (*arch != "" || *build != 0)) || uint64(*build) > 0xffffffff {
		fmt.Fprintln(os.Stderr, "Use --package DIR. --arch/--build are allowed only with --static.")
		os.Exit(2)
	}
	var env audit.Environment
	if *static {
		env = audit.Environment{Architecture: *arch, Build: uint32(*build), Static: true}
	} else {
		var e error
		env, e = platform.Environment()
		if e != nil {
			json.NewEncoder(os.Stdout).Encode(audit.Result{Schema: "S7_WINDOWS_PREFLIGHT_1", Issues: []string{e.Error()}})
			os.Exit(1)
		}
	}
	env.Component = *component
	result := audit.Verify(*root, env, platform.Trust())
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if e := enc.Encode(result); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	if !result.Passed {
		os.Exit(1)
	}
}
