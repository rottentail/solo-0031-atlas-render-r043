package service_test

import (
	"atlas-render/internal/engine"
	"atlas-render/internal/persist"
	"testing"
	"time"
)

const pngSignature = "\x89PNG\r\n\x1a\n"

func newWorker(t *testing.T, st *persist.Store) *engine.Worker {
	t.Helper()
	w := engine.New(st)
	if e := w.Recover(); e != nil {
		t.Fatalf("worker recover: %v", e)
	}
	return w
}

func deadlineChannel() <-chan time.Time {
	return time.After(10 * time.Second)
}

func waitShort() { time.Sleep(5 * time.Millisecond) }
