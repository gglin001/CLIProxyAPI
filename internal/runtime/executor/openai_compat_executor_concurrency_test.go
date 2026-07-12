package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestOpenAICompatExecutorUpstreamConcurrencyPreventsOverload(t *testing.T) {
	testCases := []struct {
		name            string
		control         *config.OpenAICompatibilityUpstreamConcurrency
		wantErrors      bool
		wantMaxInFlight int64
	}{
		{name: "uncontrolled", wantErrors: true},
		{
			name: "controlled",
			control: &config.OpenAICompatibilityUpstreamConcurrency{
				Enabled:                 true,
				MinConcurrency:          2,
				InitialConcurrency:      2,
				MaxConcurrency:          2,
				QueueSize:               16,
				QueueTimeoutSeconds:     2,
				SuccessesBeforeIncrease: 100,
			},
			wantMaxInFlight: 2,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var active atomic.Int64
			var maxActive atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				current := active.Add(1)
				defer active.Add(-1)
				for {
					previous := maxActive.Load()
					if current <= previous || maxActive.CompareAndSwap(previous, current) {
						break
					}
				}
				_, _ = io.Copy(io.Discard, request.Body)
				if current > 2 {
					w.WriteHeader(http.StatusGatewayTimeout)
					_, _ = w.Write([]byte("simulated upstream overload"))
					return
				}
				time.Sleep(40 * time.Millisecond)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			}))
			defer server.Close()

			cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
				Name:                "mirror",
				BaseURL:             server.URL + "/v1",
				UpstreamConcurrency: testCase.control,
			}}}
			executor := NewOpenAICompatExecutor("openai-compatible-mirror", cfg)
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"base_url":    server.URL + "/v1",
				"api_key":     "test",
				"compat_name": "mirror",
			}}
			auth.EnsureIndex()

			const requestCount = 8
			start := make(chan struct{})
			errorsCh := make(chan error, requestCount)
			var waitGroup sync.WaitGroup
			for index := 0; index < requestCount; index++ {
				waitGroup.Add(1)
				go func() {
					defer waitGroup.Done()
					<-start
					_, errExecute := executor.Execute(context.Background(), auth.Clone(), cliproxyexecutor.Request{
						Model:   "deepseek-v4-pro",
						Payload: []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"probe"}]}`),
					}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
					errorsCh <- errExecute
				}()
			}
			close(start)
			waitGroup.Wait()
			close(errorsCh)

			errorCount := 0
			for errExecute := range errorsCh {
				if errExecute != nil {
					errorCount++
				}
			}
			if testCase.wantErrors && errorCount == 0 {
				t.Fatal("expected uncontrolled requests to overload upstream")
			}
			if !testCase.wantErrors && errorCount != 0 {
				t.Fatalf("controlled requests returned %d errors", errorCount)
			}
			if testCase.wantMaxInFlight > 0 && maxActive.Load() > testCase.wantMaxInFlight {
				t.Fatalf("max upstream in-flight = %d, want <= %d", maxActive.Load(), testCase.wantMaxInFlight)
			}
		})
	}
}

func TestOpenAICompatExecutorConvertsConfiguredOverloadTo429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
		_, _ = w.Write([]byte("simulated gateway timeout"))
	}))
	defer server.Close()

	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name:    "mirror",
		BaseURL: server.URL + "/v1",
		UpstreamConcurrency: &config.OpenAICompatibilityUpstreamConcurrency{
			Enabled:                 true,
			MinConcurrency:          1,
			InitialConcurrency:      1,
			MaxConcurrency:          1,
			QueueSize:               4,
			QueueTimeoutSeconds:     2,
			OverloadCooldownSeconds: 1,
		},
	}}}
	executor := NewOpenAICompatExecutor("openai-compatible-mirror", cfg)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url":    server.URL + "/v1",
		"api_key":     "test",
		"compat_name": "mirror",
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-pro",
		Payload: []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"probe"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
	if err == nil {
		t.Fatal("expected overload error")
	}
	statusErr, ok := err.(interface{ StatusCode() int })
	if !ok || statusErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("error = %v, want status 429", err)
	}
	retryable, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok {
		t.Fatalf("error = %T, want RetryAfter", err)
	}
	retryAfter := retryable.RetryAfter()
	if retryAfter == nil || *retryAfter != time.Second {
		t.Fatalf("retry after = %v, want 1s", retryAfter)
	}
	if got := err.Error(); got == "" {
		t.Fatal("expected overload error message")
	} else if want := "status 504"; !strings.Contains(got, want) {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestOpenAICompatExecutorHoldsConcurrencyLeaseUntilStreamEnds(t *testing.T) {
	var active atomic.Int64
	var maxActive atomic.Int64
	var requestCount atomic.Int64
	releaseStreams := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount.Add(1)
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := maxActive.Load()
			if current <= previous || maxActive.CompareAndSwap(previous, current) {
				break
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("response writer does not support flushing")
			return
		}
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n"))
		flusher.Flush()
		<-releaseStreams
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name:    "stream-mirror",
		BaseURL: server.URL + "/v1",
		UpstreamConcurrency: &config.OpenAICompatibilityUpstreamConcurrency{
			Enabled:                 true,
			MinConcurrency:          1,
			InitialConcurrency:      1,
			MaxConcurrency:          1,
			QueueSize:               4,
			QueueTimeoutSeconds:     2,
			SuccessesBeforeIncrease: 100,
		},
	}}}
	executor := NewOpenAICompatExecutor("openai-compatible-stream-mirror", cfg)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url":    server.URL + "/v1",
		"api_key":     "test",
		"compat_name": "stream-mirror",
	}}
	request := cliproxyexecutor.Request{
		Model:   "deepseek-v4-pro",
		Payload: []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"probe"}],"stream":true}`),
	}
	options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), Stream: true}

	first, err := executor.ExecuteStream(context.Background(), auth, request, options)
	if err != nil {
		t.Fatalf("first ExecuteStream error: %v", err)
	}
	secondResult := make(chan *cliproxyexecutor.StreamResult, 1)
	secondError := make(chan error, 1)
	go func() {
		result, errExecute := executor.ExecuteStream(context.Background(), auth, request, options)
		secondResult <- result
		secondError <- errExecute
	}()

	time.Sleep(50 * time.Millisecond)
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("upstream request count before first stream ended = %d, want 1", got)
	}
	close(releaseStreams)
	for range first.Chunks {
	}

	var second *cliproxyexecutor.StreamResult
	select {
	case second = <-secondResult:
		if errExecute := <-secondError; errExecute != nil {
			t.Fatalf("second ExecuteStream error: %v", errExecute)
		}
	case <-time.After(time.Second):
		t.Fatal("second stream did not acquire capacity after first stream ended")
	}
	for range second.Chunks {
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("max active streams = %d, want 1", got)
	}
}
