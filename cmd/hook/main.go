package main

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/hingst-it/panko/internal/hook"
	"github.com/hingst-it/panko/internal/serve"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	seconds := 300.0
	if v := os.Getenv("DELETE_ALERT_SECONDS"); v != "" {
		var err error
		seconds, err = strconv.ParseFloat(v, 64)
		if err != nil {
			return err
		}
	}
	if seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds > float64(math.MaxInt64)/float64(time.Second) {
		return fmt.Errorf("invalid DELETE_ALERT_SECONDS")
	}
	return serve.Run(hook.Handler(hook.NewTelemetry(os.Stdout, time.Duration(seconds*float64(time.Second)))))
}
