package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"src.solsynth.dev/sosys/elecpostal/internal/app"
	"src.solsynth.dev/sosys/elecpostal/internal/config"
	"src.solsynth.dev/sosys/elecpostal/internal/logging"
)

func main() {
	configPath := flag.String("config", os.Getenv("CONFIG_PATH"), "config file path")
	pretty := flag.Bool("pretty", os.Getenv("ZEROLOG_PRETTY") == "true", "pretty logging")
	flag.Parse()

	logging.Init(*pretty)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logging.Log.Fatal().Err(err).Msg("failed to load config")
	}

	runner, err := app.New(cfg)
	if err != nil {
		logging.Log.Fatal().Err(err).Msg("failed to create app")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runner.Start(ctx); err != nil {
		logging.Log.Fatal().Err(err).Msg("failed to start app")
	}

	<-ctx.Done()
	if err := runner.Stop(context.Background()); err != nil {
		logging.Log.Error().Err(err).Msg("shutdown error")
	}

	fmt.Println("shutdown complete")
}
