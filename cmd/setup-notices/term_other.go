//go:build !linux

package main

import (
	"errors"
	"os"
)

func isTerminal(*os.File) bool { return false }

func readHidden(*os.File) ([]byte, error) { return nil, errors.New("hidden input unsupported") }
