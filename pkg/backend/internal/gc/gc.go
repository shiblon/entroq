// Package gc implements backend garbage collection using only the public
// Backend contract. Task and doc collection therefore inherit exactly the same
// claim and modification semantics as ordinary workers.
package gc

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/queues"
)

const claimant = "entroq-gc-collector"

type Reporter interface {
	Deleted(context.Context, string, int)
	Error(context.Context, string, string)
}

// CollectTasksOnce discovers gc=-marked queues and claim-deletes up to batch
// available tasks. A contended queue does not prevent collection elsewhere.
func CollectTasksOnce(ctx context.Context, backend entroq.Backend, batch int, reporter Reporter) (int, error) {
	now, err := backend.Time(ctx)
	if err != nil {
		return 0, fmt.Errorf("task gc time: %w", err)
	}
	names, err := backend.Queues(ctx, &entroq.MatchQuery{})
	if err != nil {
		return 0, fmt.Errorf("task gc list queues: %w", err)
	}

	var due []string
	for queue := range names {
		activateAt, present, err := queues.GCActivation(queue)
		if err != nil {
			reporter.Error(ctx, queue, "malformed")
			log.Printf("task gc: queue %q has a malformed gc= value; it will never be collected", queue)
			continue
		}
		if !present || activateAt.After(now) {
			continue
		}
		due = append(due, queue)
	}

	deleted := 0
	for _, queue := range due {
		if deleted >= batch {
			break
		}
		for deleted < batch {
			task, err := backend.TryClaim(ctx, &entroq.ClaimQuery{
				Queues:   []string{queue},
				Claimant: claimant,
				Duration: entroq.DefaultClaimDuration,
			})
			if err != nil {
				reporter.Error(ctx, queue, "claim")
				return deleted, fmt.Errorf("task gc claim %q: %w", queue, err)
			}
			if task == nil {
				break
			}
			if _, err := backend.Modify(ctx, entroq.NewModification(claimant, task.Delete())); err != nil {
				if _, ok := entroq.AsDependency(err); ok {
					continue
				}
				reporter.Error(ctx, queue, "delete")
				return deleted, fmt.Errorf("task gc delete %v: %w", task.IDVersion(), err)
			}
			deleted++
			reporter.Deleted(ctx, queue, 1)
		}
	}
	return deleted, nil
}

type docCandidate struct {
	namespace string
	key       string
}

// CollectDocsOnce discovers gc=-marked doc primary keys and deletes up to batch
// complete groups. A claimed group is skipped without affecting other keys.
func CollectDocsOnce(ctx context.Context, backend entroq.Backend, batch int, reporter Reporter) (int, error) {
	now, err := backend.Time(ctx)
	if err != nil {
		return 0, fmt.Errorf("doc gc time: %w", err)
	}
	namespaces, err := backend.NamespaceStats(ctx, &entroq.MatchQuery{})
	if err != nil {
		return 0, fmt.Errorf("doc gc list namespaces: %w", err)
	}

	var candidates []docCandidate
	for namespace := range namespaces {
		docs, err := backend.Docs(ctx, &entroq.DocQuery{Namespace: namespace, OmitValues: true})
		if err != nil {
			return 0, fmt.Errorf("doc gc list namespace %q: %w", namespace, err)
		}
		last := ""
		for _, doc := range docs {
			if doc.Key != last && strings.Contains(doc.Key, "/gc=") {
				candidates = append(candidates, docCandidate{namespace: namespace, key: doc.Key})
			}
			last = doc.Key
		}
	}

	var due []docCandidate
	for _, candidate := range candidates {
		activateAt, present, err := queues.GCActivation(candidate.key)
		if err != nil {
			reporter.Error(ctx, candidate.key, "malformed_doc_key")
			log.Printf("doc gc: key %q in namespace %q has a malformed gc= value; it will never be collected", candidate.key, candidate.namespace)
			continue
		}
		if !present || activateAt.After(now) {
			continue
		}
		due = append(due, candidate)
	}

	collected := 0
	for _, candidate := range due {
		if collected >= batch {
			break
		}
		docs, err := backend.ClaimDocs(ctx, &entroq.DocClaim{
			Namespace: candidate.namespace,
			Key:       candidate.key,
			Claimant:  claimant,
			Duration:  entroq.DefaultClaimDuration,
		})
		if err != nil {
			if _, ok := entroq.AsDependency(err); ok {
				continue
			}
			return collected, fmt.Errorf("doc gc claim %q/%q: %w", candidate.namespace, candidate.key, err)
		}
		if len(docs) == 0 {
			continue
		}
		deletes := make([]entroq.ModifyArg, 0, len(docs))
		for _, doc := range docs {
			deletes = append(deletes, doc.Delete())
		}
		if _, err := backend.Modify(ctx, entroq.NewModification(claimant, deletes...)); err != nil {
			if _, ok := entroq.AsDependency(err); ok {
				continue
			}
			return collected, fmt.Errorf("doc gc delete %q/%q: %w", candidate.namespace, candidate.key, err)
		}
		collected++
		reporter.Deleted(ctx, candidate.key, len(docs))
	}
	return collected, nil
}
