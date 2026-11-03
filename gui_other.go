//go:build !windows

package main

import "errors"

func attachConsole() {}

func runGUI() error {
	return errors.New("the graphical interface is only available on Windows; use the command-line flags")
}
