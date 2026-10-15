package main

import (
	"os"

	controllerruntime "github.com/kerolasa/teleport-sre/internal/controller-runtime"
)

func main() {
	os.Exit(controllerruntime.Run())
}
