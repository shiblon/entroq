package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/shiblon/entroq/pkg/worker"
)

const (
	configQueue = "/config"
	workQueue   = "/work"
	outboxQueue = "/results"
)

// Config is the shared configuration stored in EntroQ.
type Config struct {
	Multiplier int `json:"multiplier"`
}

// ConfigWorker holds worker-local configuration state.
// Because worker.Run executes the loop sequentially, this state is safe
// from races without locks as long as one ConfigWorker belongs to one Run.
type ConfigWorker struct {
	eqc *entroq.EntroQ

	config Config
	cID    *entroq.TaskID
}

// reloadConfig refreshes the local configuration from EntroQ.
func (cw *ConfigWorker) reloadConfig(ctx context.Context) error {
	task, err := cw.eqc.Claim(ctx, entroq.From(configQueue), entroq.ClaimFor(time.Second))
	if err != nil {
		return fmt.Errorf("reload claim: %w", err)
	}

	cfg, err := entroq.GetValue[Config](task)
	if err != nil {
		return fmt.Errorf("reload unmarshal: %w", err)
	}

	cw.config = cfg
	cw.cID = task.IDVersion()

	// Immediately release the configuration task.
	if _, err := cw.eqc.Modify(ctx, entroq.Changing(task, entroq.ArrivalTimeBy(0))); err != nil {
		return fmt.Errorf("reload release: %w", err)
	}

	log.Printf("[Worker] Refreshed local config: Multiplier=%v (Version=%v)", cfg.Multiplier, cw.cID)
	return nil
}

// handleDependencyError is the hook called when a task modification fails.
func (cw *ConfigWorker) handleDependencyError(ctx context.Context, task *entroq.Task, de *entroq.DependencyError) error {
	log.Printf("[Worker] Dependency failure! Local version %v is stale. Re-syncing...", cw.cID)
	return cw.reloadConfig(ctx)
}

// doWork processes a single numeric task using the current local multiplier.
func (cw *ConfigWorker) doWork(ctx context.Context, t *entroq.Task, val int, _ []*entroq.Doc) ([]entroq.ModifyArg, error) {
	// We use the LOCAL state here. Even if another worker instance refreshed
	// their own config, ours stays what it was when it was read.
	multiplier := cw.config.Multiplier

	result := val * multiplier
	log.Printf("[Worker] Processing: %v * %v = %v (ver %v)", val, multiplier, result, cw.cID)

	return []entroq.ModifyArg{
		t.Delete(),
		cw.cID.Depend(),
		entroq.InsertingInto(outboxQueue, entroq.WithValue(result)),
	}, nil
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Setup EntroQ
	client, err := entroq.New(ctx, eqmem.Opener())
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	// Initialize Config Task
	if _, err := client.Modify(ctx, entroq.InsertingInto(configQueue, entroq.WithValue(Config{Multiplier: 2}))); err != nil {
		log.Fatalf("Failed to init config: %v", err)
	}

	// Create a Local Worker Instance
	cw := &ConfigWorker{eqc: client}
	if err := cw.reloadConfig(ctx); err != nil {
		log.Fatalf("Initial load: %v", err)
	}

	// Background Config Updater
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
				task, err := client.Claim(ctx, entroq.From(configQueue), entroq.ClaimFor(time.Second))
				if err != nil {
					log.Printf("[Updater] Claim error: %v", err)
					continue
				}
				newMultiplier := rand.Intn(10) + 1
				log.Printf("[Updater] Setting new multiplier: %v", newMultiplier)

				valBytes, err := json.Marshal(Config{Multiplier: newMultiplier})
				if err != nil {
					log.Printf("[Updater] Marshal error: %v", err)
					continue
				}

				if _, err := client.Modify(ctx, task.Change(entroq.RawValueTo(valBytes), entroq.ArrivalTimeBy(0))); err != nil {
					log.Printf("[Updater] Update error: %v", err)
				}
			}
		}
	}()

	// Task Producer
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
				val := rand.Intn(100)
				if _, err := client.Modify(ctx, entroq.InsertingInto(workQueue, entroq.WithValue(val))); err != nil {
					log.Printf("[Producer] Task error: %v", err)
				}
			}
		}
	}()

	// Do work
	log.Printf("Starting worker...")
	w := worker.New(client, worker.WithDoModify(cw.doWork))
	if err := w.Run(ctx,
		worker.Watching(workQueue),
		worker.WithDependencyHandler(cw.handleDependencyError),
	); err != nil {
		log.Printf("...Worker stopped: %v", err)
	}
}
