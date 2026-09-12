package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: probe <serve|load|hold>")
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "load":
		err = load(os.Args[2:])
	case "hold":
		hold()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}

func hold() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	<-signals
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flags.String("addr", ":8000", "listen address")
	if err := flags.Parse(args); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/duplex", func(w http.ResponseWriter, r *http.Request) {
		controller := http.NewResponseController(w)
		if err := controller.EnableFullDuplex(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		if err := controller.Flush(); err != nil {
			return
		}

		reader := bufio.NewReaderSize(r.Body, 64<<10)
		for {
			frame, err := reader.ReadBytes('\n')
			if len(frame) > 0 {
				if _, writeErr := w.Write(frame); writeErr != nil {
					return
				}
				if flushErr := controller.Flush(); flushErr != nil {
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
					log.Printf("request ended: %v", err)
				}
				return
			}
		}
	})

	server := &http.Server{Addr: *addr, Handler: mux}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.ListenAndServe()
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("serving full-duplex echo on %s", *addr)
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown echo server: %w", err)
		}
		return nil
	}
}

func load(args []string) error {
	flags := flag.NewFlagSet("load", flag.ContinueOnError)
	url := flags.String("url", "http://localhost:8080/duplex", "EQLink sender URL")
	host := flags.String("host", "receiver.test", "HTTP Host routed by EQLink")
	sessions := flags.Int("sessions", 24, "concurrent sessions")
	frames := flags.Int("frames", 8, "frames per session")
	payloadBytes := flags.Int("payload-bytes", 8192, "base frame payload size")
	delay := flags.Duration("delay", 5*time.Millisecond, "pause between acknowledged frames")
	timeout := flags.Duration("timeout", 45*time.Second, "whole-run timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *sessions < 1 || *frames < 1 || *payloadBytes < 1 || *delay < 0 || *timeout <= 0 {
		return errors.New("sessions, frames, payload-bytes, and timeout must be positive; delay must be non-negative")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client := &http.Client{Transport: &http.Transport{
		DisableCompression: true,
		ForceAttemptHTTP2:  false,
		MaxConnsPerHost:    *sessions,
	}}
	defer client.CloseIdleConnections()

	started := time.Now()
	errCh := make(chan error, *sessions)
	var group sync.WaitGroup
	for session := range *sessions {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := runSession(ctx, client, *url, *host, session, *frames, *payloadBytes, *delay); err != nil {
				errCh <- err
			}
		}()
	}
	group.Wait()
	close(errCh)

	var first error
	failures := 0
	for err := range errCh {
		failures++
		if first == nil {
			first = err
		}
		log.Printf("session failure: %v", err)
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d sessions failed; first: %w", failures, *sessions, first)
	}
	log.Printf("PASS sessions=%d frames=%d payload=%d elapsed=%s", *sessions, *frames, *payloadBytes, time.Since(started).Round(time.Millisecond))
	return nil
}

func runSession(parent context.Context, client *http.Client, url, host string, session, frames, payloadBytes int, delay time.Duration) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	requestBody, requestWriter := io.Pipe()
	defer requestBody.Close()
	defer requestWriter.Close()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, requestBody)
	if err != nil {
		return fmt.Errorf("session %d request: %w", session, err)
	}
	request.Host = host

	type responseResult struct {
		response *http.Response
		err      error
	}
	responseReady := make(chan responseResult, 1)
	go func() {
		response, requestErr := client.Do(request)
		responseReady <- responseResult{response: response, err: requestErr}
	}()

	var response *http.Response
	var responseBody *bufio.Reader
	for frameNumber := range frames {
		frame := makeFrame(session, frameNumber, payloadBytes)
		if _, err := requestWriter.Write(frame); err != nil {
			return fmt.Errorf("session %d frame %d write: %w", session, frameNumber, err)
		}
		if response == nil {
			select {
			case result := <-responseReady:
				if result.err != nil {
					return fmt.Errorf("session %d response: %w", session, result.err)
				}
				response = result.response
				defer response.Body.Close()
				if response.StatusCode != http.StatusOK {
					return fmt.Errorf("session %d status: %s", session, response.Status)
				}
				responseBody = bufio.NewReaderSize(response.Body, 64<<10)
			case <-ctx.Done():
				return fmt.Errorf("session %d response headers: %w", session, ctx.Err())
			}
		}
		got, err := responseBody.ReadBytes('\n')
		if err != nil {
			return fmt.Errorf("session %d frame %d read: %w", session, frameNumber, err)
		}
		if !bytes.Equal(got, frame) {
			return fmt.Errorf("session %d frame %d mismatch: got %d bytes, want %d", session, frameNumber, len(got), len(frame))
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("session %d frame %d delay: %w", session, frameNumber, ctx.Err())
			}
		}
	}
	if err := requestWriter.Close(); err != nil {
		return fmt.Errorf("session %d close request: %w", session, err)
	}
	trailing, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("session %d finish response: %w", session, err)
	}
	if len(trailing) != 0 {
		return fmt.Errorf("session %d unexpected trailing bytes: %d", session, len(trailing))
	}
	return nil
}

func makeFrame(session, frame, payloadBytes int) []byte {
	size := payloadBytes + (session+frame)%257
	if frame == 1 && session%6 == 0 {
		size = 96<<10 + session
	}
	prefix := fmt.Sprintf("session=%03d frame=%03d ", session, frame)
	result := make([]byte, 0, len(prefix)+size+1)
	result = append(result, prefix...)
	result = append(result, bytes.Repeat([]byte{'a' + byte(session%26)}, size)...)
	return append(result, '\n')
}
