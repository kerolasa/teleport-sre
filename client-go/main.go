package main

import (
	"os"

	clientgo "github.com/kerolasa/teleport-sre/internal/client-go"
)

func main() {
	os.Exit(clientgo.Run())
}
