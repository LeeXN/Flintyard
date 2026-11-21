package main

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// Native build test fixture only. Never included in product images or CLI.
func main() {
	if os.Getpid() != 1 {
		fmt.Fprintln(os.Stderr, "step is not PID 1")
		os.Exit(10)
	}
	if len(os.Args) < 2 {
		os.Exit(11)
	}
	if os.Args[1] == "-a" && len(os.Args) == 3 {
		if err := os.WriteFile("/lib/modules/"+os.Args[2]+"/modules.dep", []byte("kernel/fixture.ko:\n"), 0644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(17)
		}
		return
	}
	if len(os.Args) > 2 {
		if _, err := os.Stat(os.Args[2]); err == nil {
			fmt.Fprintln(os.Stderr, "host path visible from guest step")
			os.Exit(12)
		}
		if _, err := os.Stat("/proc/1/root" + os.Args[2]); err == nil {
			fmt.Fprintln(os.Stderr, "host root visible through proc")
			os.Exit(13)
		}
	}
	switch os.Args[1] {
	case "write":
		if err := syscall.Sethostname([]byte("isolated-build-fixture")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(16)
		}
		if err := os.WriteFile("/made-by-step", []byte("real isolated build step\n"), 0644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(14)
		}
	case "wait":
		if err := os.WriteFile("/waiting-step", []byte("isolated step running\n"), 0644); err != nil {
			os.Exit(20)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "initrd":
		if err := os.MkdirAll("/boot", 0755); err != nil {
			os.Exit(18)
		}
		if err := os.WriteFile("/boot/fixture-initrd", []byte("synthetic initrd fixture\n"), 0644); err != nil {
			os.Exit(19)
		}
	default:
		os.Exit(15)
	}
}
