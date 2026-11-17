package main

import (
	_ "embed"
	"os"
	"os/exec"
)

//go:embed gui.py
var guiScript string

// cmdGUI runs the embedded GTK4 window, which shells back out to this binary.
func cmdGUI(args []string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command("python3", append([]string{"-c", guiScript}, args...)...)
	cmd.Env = append(os.Environ(), "TSDROP_BIN="+self)
	if os.Getenv("GSK_RENDERER") == "" {
		cmd.Env = append(cmd.Env, "GSK_RENDERER=gl") // no Vulkan GPU here; skip the noisy probe
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
