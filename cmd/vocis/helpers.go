package main

import "os/exec"

func findExecutable(name string) (string, bool) {
	path, err := exec.LookPath(name)
	return path, err == nil
}
