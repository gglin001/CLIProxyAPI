package helps

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestUpstreamConcurrencyQueuesUntilLeaseCompletes(t *testing.T) {
	upstreamConcurrencyControllers = sync.Map{}
	cfg := &config.OpenAICompatibilityUpstreamConcurrency{
		Enabled:             true,
		MinConcurrency:      2,
		InitialConcurrency:  2,
		MaxConcurrency:      2,
		QueueSize:           4,
		QueueTimeoutSeconds: 1,
	}
	first, err := AcquireUpstreamConcurrency(context.Background(), "queue-test", cfg)
	if err != nil {
		t.Fatalf("first acquire error: %v", err)
	}
	second, err := AcquireUpstreamConcurrency(context.Background(), "queue-test", cfg)
	if err != nil {
		t.Fatalf("second acquire error: %v", err)
	}

	thirdResult := make(chan *UpstreamConcurrencyLease, 1)
	thirdError := make(chan error, 1)
	go func() {
		third, errAcquire := AcquireUpstreamConcurrency(context.Background(), "queue-test", cfg)
		thirdResult <- third
		thirdError <- errAcquire
	}()

	select {
	case <-thirdResult:
		t.Fatal("third acquire completed before capacity was released")
	case <-time.After(25 * time.Millisecond):
	}

	first.FinishSuccess()
	select {
	case third := <-thirdResult:
		if errAcquire := <-thirdError; errAcquire != nil {
			t.Fatalf("third acquire error: %v", errAcquire)
		}
		third.FinishSuccess()
	case <-time.After(time.Second):
		t.Fatal("third acquire did not resume after capacity was released")
	}
	second.FinishSuccess()
}

func TestUpstreamConcurrencyOverloadHalvesLimitAndConvertsStatus(t *testing.T) {
	upstreamConcurrencyControllers = sync.Map{}
	cfg := &config.OpenAICompatibilityUpstreamConcurrency{
		Enabled:             true,
		MinConcurrency:      1,
		InitialConcurrency:  4,
		MaxConcurrency:      8,
		QueueSize:           4,
		QueueTimeoutSeconds: 1,
	}
	lease, err := AcquireUpstreamConcurrency(context.Background(), "overload-test", cfg)
	if err != nil {
		t.Fatalf("acquire error: %v", err)
	}
	overloadErr := lease.FinishHTTPFailure(http.StatusGatewayTimeout, "gateway timeout")
	if overloadErr == nil {
		t.Fatal("expected converted overload error")
	}
	statusErr, ok := overloadErr.(interface{ StatusCode() int })
	if !ok || statusErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status error = %#v, want 429", overloadErr)
	}

	value, ok := upstreamConcurrencyControllers.Load("overload-test")
	if !ok {
		t.Fatal("controller missing")
	}
	controller := value.(*upstreamConcurrencyController)
	controller.mu.Lock()
	limit := controller.limit
	controller.mu.Unlock()
	if limit != 2 {
		t.Fatalf("limit = %d, want 2", limit)
	}
}

func TestUpstreamConcurrencyRejectsWhenQueueIsFull(t *testing.T) {
	upstreamConcurrencyControllers = sync.Map{}
	cfg := &config.OpenAICompatibilityUpstreamConcurrency{
		Enabled:             true,
		MinConcurrency:      1,
		InitialConcurrency:  1,
		MaxConcurrency:      1,
		QueueSize:           1,
		QueueTimeoutSeconds: 1,
	}
	active, err := AcquireUpstreamConcurrency(context.Background(), "full-test", cfg)
	if err != nil {
		t.Fatalf("active acquire error: %v", err)
	}
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	defer cancelWaiter()
	waiterDone := make(chan error, 1)
	go func() {
		_, errAcquire := AcquireUpstreamConcurrency(waiterCtx, "full-test", cfg)
		waiterDone <- errAcquire
	}()

	deadline := time.Now().Add(time.Second)
	for {
		value, _ := upstreamConcurrencyControllers.Load("full-test")
		controller := value.(*upstreamConcurrencyController)
		controller.mu.Lock()
		queued := controller.queued
		controller.mu.Unlock()
		if queued == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiter did not enter queue")
		}
		time.Sleep(time.Millisecond)
	}

	_, fullErr := AcquireUpstreamConcurrency(context.Background(), "full-test", cfg)
	if fullErr == nil {
		t.Fatal("expected queue full error")
	}
	statusErr, ok := fullErr.(interface{ StatusCode() int })
	if !ok || statusErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("queue full error = %#v, want 429", fullErr)
	}
	cancelWaiter()
	<-waiterDone
	active.FinishNeutral()
}
