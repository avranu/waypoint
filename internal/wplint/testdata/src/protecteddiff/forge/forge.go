package forge

import (
	"os/exec"
	"strings"
)

func protectedPaths(names []string) []string { return names }

func ChangedBad(base string) []string {
	out, _ := exec.Command("git", "diff", "--name-only", base).Output() // want `follows renames` `without -z`
	return protectedPaths(strings.Split(string(out), "\n"))
}

func ChangedGood(base string) []string {
	out, _ := exec.Command("git", "diff", "--no-renames", "--name-only", "-z", base).Output()
	return protectedPaths(strings.Split(string(out), "\x00"))
}

func Unrelated(base string) error {
	return exec.Command("git", "diff", "--name-only", base).Run()
}
