package taskstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"agent-gateway/internal/protocol"
)

// TestConcurrentClaimHandsOutOneTaskOnce is the claim race: many nodes poll at
// once and exactly one of them may receive the single queued task.
func TestConcurrentClaimHandsOutOneTaskOnce(t *testing.T) {
	st := newStore(t)
	task := mustSubmit(t, st, submitInput())

	const workers = 16
	leases := make([]*protocol.Lease, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			leases[i], errs[i] = st.Claim(context.Background(), testNode, time.Minute)
		}(i)
	}
	wg.Wait()

	claimed := 0
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Errorf("worker %d: %v", i, errs[i])
			continue
		}
		if leases[i] == nil {
			continue
		}
		claimed++
		if leases[i].Task.ID != task.ID {
			t.Errorf("worker %d claimed %s, want %s", i, leases[i].Task.ID, task.ID)
		}
		if leases[i].Token == "" {
			t.Errorf("worker %d received an empty token", i)
		}
	}
	if claimed != 1 {
		t.Fatalf("%d of %d workers were handed the single task, want exactly 1", claimed, workers)
	}
	if n := countState(t, st, protocol.Leased); n != 1 {
		t.Fatalf("%d tasks are leased, want 1", n)
	}
}

func TestConcurrentClaimDistributesDistinctTasks(t *testing.T) {
	const tasks = 8
	const workers = 16
	st := newStore(t)
	for i := 0; i < tasks; i++ {
		mustSubmit(t, st, submitInput())
	}

	leases := make([]*protocol.Lease, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			leases[i], errs[i] = st.Claim(context.Background(), testNode, time.Minute)
		}(i)
	}
	wg.Wait()

	seen := map[string]int{}
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Errorf("worker %d: %v", i, errs[i])
			continue
		}
		if leases[i] == nil {
			continue
		}
		seen[leases[i].Task.ID]++
	}
	if len(seen) != tasks {
		t.Fatalf("%d distinct tasks were claimed, want %d", len(seen), tasks)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("task %s was handed out %d times", id, n)
		}
	}
	if n := countState(t, st, protocol.Leased); n != tasks {
		t.Fatalf("%d tasks are leased, want %d", n, tasks)
	}
}

func TestConcurrentSubmitWithSameKey(t *testing.T) {
	const workers = 12
	st := newStore(t)
	in := submitInput()
	in.IdempotencyKey = "shared-key"

	results := make([]protocol.Task, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = st.Submit(context.Background(), in)
		}(i)
	}
	wg.Wait()

	want := results[0].ID
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Errorf("worker %d: %v", i, errs[i])
			continue
		}
		if results[i].ID != want {
			t.Errorf("worker %d got task %s, want the single task %s", i, results[i].ID, want)
		}
	}
	if count := countTasks(t, st); count != 1 {
		t.Fatalf("stored %d tasks for one idempotency key, want 1", count)
	}
}

func TestConcurrentSubmitWithDifferentPayloads(t *testing.T) {
	const workers = 8
	st := newStore(t)

	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := submitInput()
			in.IdempotencyKey = "contested-key"
			in.Input = json.RawMessage(fmt.Sprintf(`{"worker":%d}`, i))
			_, errs[i] = st.Submit(context.Background(), in)
		}(i)
	}
	wg.Wait()

	succeeded, conflicted := 0, 0
	for i := 0; i < workers; i++ {
		switch {
		case errs[i] == nil:
			succeeded++
		case errors.Is(errs[i], protocol.ErrConflict):
			conflicted++
		default:
			t.Errorf("worker %d: %v", i, errs[i])
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d submissions succeeded, want exactly 1", succeeded)
	}
	if conflicted != workers-1 {
		t.Fatalf("%d submissions conflicted, want %d", conflicted, workers-1)
	}
	if count := countTasks(t, st); count != 1 {
		t.Fatalf("stored %d tasks for one idempotency key, want 1", count)
	}
}

// TestConcurrentCancelAndComplete runs the two mutations a cancellation
// race produces. Either order is correct; what must never happen is a task
// that reports success and a cancellation request at the same time.
func TestConcurrentCancelAndComplete(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	task, lease := claimStart(t, st)

	var wg sync.WaitGroup
	var cancelErr, completeErr error
	var cancelled protocol.Task
	wg.Add(2)
	go func() {
		defer wg.Done()
		cancelled, cancelErr = st.Cancel(ctx, task.ID)
	}()
	go func() {
		defer wg.Done()
		completeErr = st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, baseResult())
	}()
	wg.Wait()

	if cancelErr != nil {
		t.Fatalf("Cancel: %v", cancelErr)
	}
	if completeErr != nil && !errors.Is(completeErr, protocol.ErrConflict) {
		t.Fatalf("Complete: %v, want nil or protocol.ErrConflict", completeErr)
	}
	final := mustGet(t, st, task.ID)
	switch final.State {
	case protocol.Succeeded:
		if completeErr != nil {
			t.Fatalf("the task succeeded but Complete reported %v", completeErr)
		}
		if cancelled.State != protocol.Succeeded {
			t.Fatalf("Cancel returned state %s after Complete won the race", cancelled.State)
		}
	case protocol.CancelRequested:
		if !errors.Is(completeErr, protocol.ErrConflict) {
			t.Fatalf("Complete = %v, want protocol.ErrConflict after the cancellation won the race", completeErr)
		}
		if cancelled.State != protocol.CancelRequested {
			t.Fatalf("Cancel returned state %s, want cancel_requested", cancelled.State)
		}
		if final.Result != nil {
			t.Fatalf("a task awaiting cancellation has a result: %+v", final.Result)
		}
	default:
		t.Fatalf("final state = %s, want succeeded or cancel_requested", final.State)
	}
}

// TestConcurrentExpireAndComplete races a completion against the expiry of the
// lease it depends on. The lease is still valid for the store's clock while
// the expiry sweep runs with a later instant, so both orders are reachable.
func TestConcurrentExpireAndComplete(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	base := time.Now().UTC()
	freezeClock(st, base)
	task, lease := claimStart(t, st)

	var wg sync.WaitGroup
	var moved int64
	var expireErr, completeErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		moved, expireErr = st.Expire(ctx, base.Add(2*time.Minute))
	}()
	go func() {
		defer wg.Done()
		completeErr = st.Complete(ctx, task.ID, lease.Task.AttemptID, lease.Token, baseResult())
	}()
	wg.Wait()

	if expireErr != nil {
		t.Fatalf("Expire: %v", expireErr)
	}
	final := mustGet(t, st, task.ID)
	switch final.State {
	case protocol.Unknown:
		if !errors.Is(completeErr, protocol.ErrConflict) {
			t.Fatalf("Complete on an expired task = %v, want protocol.ErrConflict", completeErr)
		}
		if moved != 1 {
			t.Fatalf("Expire moved %d tasks, want 1", moved)
		}
		if final.Result != nil {
			t.Fatalf("an unknown task has a result: %+v", final.Result)
		}
	case protocol.Succeeded:
		if completeErr != nil {
			t.Fatalf("the task succeeded but Complete reported %v", completeErr)
		}
		if moved != 0 {
			t.Fatalf("Expire moved %d tasks after the task had already finished", moved)
		}
	default:
		t.Fatalf("final state = %s, want unknown or succeeded", final.State)
	}
}
