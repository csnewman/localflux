package main

import (
	"log/slog"

	"github.com/csnewman/localflux/internal/config"
)

func main() {
	c, err := config.Load("localflux.yaml")
	if err != nil {
		panic(err)
	}

	slog.Info("Config", "c", c)
}
