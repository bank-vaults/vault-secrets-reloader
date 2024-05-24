// Copyright © 2023 Cisco
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"time"

	slogmulti "github.com/samber/slog-multi"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	"github.com/bank-vaults/vault-secrets-reloader/pkg/reloader"
)

const (
	defaultSyncPeriod        = 30 * time.Second
	defaultReloaderRunPeriod = 60 * time.Second
)

func main() {
	// Register CLI flags
	collectorSyncPeriod := flag.Duration("collector-sync-period", defaultSyncPeriod,
		"Determines the minimum frequency at which watched resources are reconciled")
	reloaderRunPeriod := flag.Duration("reloader-run-period", defaultReloaderRunPeriod,
		"Determines the minimum frequency at which watched resources are reloaded")
	logLevel := flag.String("log-level", "info", "Log level (debug, info, warn, error).")
	enableJSONLog := flag.Bool("enable-json-log", false, "Enable JSON logging")
	flag.Parse()

	// Set up signals so we handle the shutdown signal gracefully
	ctx := signals.SetupSignalHandler()

	// Setup logger
	var logger *slog.Logger
	{
		var level slog.Level

		err := level.UnmarshalText([]byte(*logLevel))
		if err != nil { // Silently fall back to info level
			level = slog.LevelInfo
		}

		levelFilter := func(levels ...slog.Level) func(ctx context.Context, r slog.Record) bool {
			return func(_ context.Context, r slog.Record) bool {
				return slices.Contains(levels, r.Level)
			}
		}

		router := slogmulti.Router()

		if *enableJSONLog {
			// Send logs with level higher than warning to stderr
			router = router.Add(
				slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}),
				levelFilter(slog.LevelWarn, slog.LevelError),
			)

			// Send info and debug logs to stdout
			router = router.Add(
				slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}),
				levelFilter(slog.LevelDebug, slog.LevelInfo),
			)
		} else {
			// Send logs with level higher than warning to stderr
			router = router.Add(
				slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}),
				levelFilter(slog.LevelWarn, slog.LevelError),
			)

			// Send info and debug logs to stdout
			router = router.Add(
				slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}),
				levelFilter(slog.LevelDebug, slog.LevelInfo),
			)
		}

		// TODO: add level filter handler
		logger = slog.New(router.Handler())
		logger = logger.With(slog.String("app", "vault-secrets-reloader"))

		slog.SetDefault(logger)
	}

	// Handler for health checks
	port := os.Getenv("LISTEN_ADDRESS")
	if port == "" {
		port = ":8080"
	}

	go func() {
		_ = http.ListenAndServe(port, http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("ok"))
			},
		))
	}()

	// Create kubernetes client
	kubeConfig, err := config.GetConfig()
	if err != nil {
		logger.Error(fmt.Errorf("error building kubeconfig: %s", err).Error())
		os.Exit(1)
	}

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		logger.Error(fmt.Errorf("error building kubernetes clientset: %s", err).Error())
		os.Exit(1)
	}

	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, *collectorSyncPeriod)

	controller := reloader.NewController(
		logger,
		kubeClient,
		kubeInformerFactory.Apps().V1().Deployments(),
		kubeInformerFactory.Apps().V1().DaemonSets(),
		kubeInformerFactory.Apps().V1().StatefulSets(),
	)

	kubeInformerFactory.Start(ctx.Done())

	if err = controller.Run(ctx, *reloaderRunPeriod); err != nil {
		logger.Error(fmt.Errorf("error running controller: %s", err).Error())
		os.Exit(1)
	}
}
