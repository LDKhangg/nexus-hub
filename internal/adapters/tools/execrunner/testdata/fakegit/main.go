// Command fakegit is a controllable stand-in for git used by runner tests.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	args := os.Args[1:]
	sub := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" || args[i] == "-C" {
			i++
			continue
		}
		if strings.HasPrefix(args[i], "-") {
			continue
		}
		sub = args[i]
		break
	}
	switch sub {
	case "log": // echo argv
		fmt.Print(strings.Join(args, "\x00"))
	case "status": // dump env
		fmt.Print(strings.Join(os.Environ(), "\n"))
	case "show": // spawn a grandchild that records its pid, then hang
		if len(args) > 0 && args[len(args)-1] == "grandchild" {
			time.Sleep(time.Hour)
			return
		}
		c := exec.Command(os.Args[0], "show", "grandchild")
		if err := c.Start(); err != nil {
			os.Exit(3)
		}
		_ = os.WriteFile(args[len(args)-1], []byte(fmt.Sprint(c.Process.Pid)), 0o600)
		time.Sleep(time.Hour)
	case "diff": // 3 MiB of output
		b := []byte(strings.Repeat("x", 1024))
		for i := 0; i < 3*1024; i++ {
			os.Stdout.Write(b)
		}
	case "grep": // burn CPU
		n := 0
		for {
			n++
		}
	case "rev-parse": // report limits and niceness after the runner applied them
		time.Sleep(200 * time.Millisecond)
		b, _ := os.ReadFile("/proc/self/limits")
		stat, _ := os.ReadFile("/proc/self/stat")
		f := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+2:]))
		fmt.Printf("%s\nnice=%s\n", b, f[16])
	case "blame": // short sleep
		time.Sleep(300 * time.Millisecond)
	default:
		os.Exit(2)
	}
}
