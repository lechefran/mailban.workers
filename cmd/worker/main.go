// Command worker runs Mailban's background jobs against the API's PostgreSQL
// database and serves GET /health for the platform's health check.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lechefran/mailban.workers/internal/db"
	"github.com/lechefran/mailban.workers/internal/scheduler"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}

	stop, cancelStop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancelStop()

	conn, err := db.Open(stop, cfg.dbConn)
	if err != nil {
		return err
	}
	defer conn.Close()

	schemaCtx, cancelSchema := context.WithTimeout(stop, 5*time.Second)
	defer cancelSchema()
	if err := db.CheckSchema(schemaCtx, conn); err != nil {
		return err
	}

	sched := scheduler.New(conn, buildJobs(cfg))
	for _, j := range sched.Jobs() {
		log.Printf("scheduling %s every %s", j.Name, j.Interval)
	}

	srv := &http.Server{
		Addr:              ":" + cfg.port,
		Handler:           healthHandler(sched),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	jobsDone := make(chan struct{})
	go func() {
		sched.Run(stop)
		close(jobsDone)
	}()
	errCh := make(chan error, 1)
	go func() {
		log.Println("starting health server on port " + cfg.port)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		cancelStop()
		<-jobsDone
		return err
	case <-stop.Done():
	}

	log.Println("shutting down worker")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	err = srv.Shutdown(shutdownCtx)
	// In-flight runs see the cancelled context, record their end and release their locks.
	<-jobsDone
	return err
}
