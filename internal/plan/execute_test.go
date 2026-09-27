package plan

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type call struct{ verb, path string }

type fakeWriter struct {
	calls []call
	err   error
}

func (f *fakeWriter) record(verb, path string) error {
	f.calls = append(f.calls, call{verb, path})
	return f.err
}

func (f *fakeWriter) Post(_ context.Context, path string, _, _ any) error {
	return f.record("PUT", path)
}
func (f *fakeWriter) Patch(_ context.Context, path string, _, _ any) error {
	return f.record("PATCH", path)
}
func (f *fakeWriter) Delete(_ context.Context, path string) error {
	return f.record("DELETE", path)
}
func (f *fakeWriter) Command(_ context.Context, path string, _, _ any) error {
	return f.record("POST", path)
}

// Each Method must reach the client call that sends that HTTP verb.
func TestExecuteDispatchesByMethod(t *testing.T) {
	for _, m := range []Method{MethodCreate, MethodUpdate, MethodDelete, MethodCommand} {
		w := &fakeWriter{}
		if err := Execute(context.Background(), w, Op{Router: "b", Method: m, Path: "/x"}); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		if len(w.calls) != 1 || w.calls[0].verb != string(m) || w.calls[0].path != "/x" {
			t.Errorf("%s: calls = %+v", m, w.calls)
		}
	}
}

func TestExecuteWrapsErrorWithRouter(t *testing.T) {
	w := &fakeWriter{err: errors.New("status 400")}
	err := Execute(context.Background(), w, Op{Router: "a", Method: MethodDelete, Path: "/x/*1"})
	if err == nil || !strings.Contains(err.Error(), "router a") {
		t.Fatalf("err = %v, want it to name router a", err)
	}
}

func TestExecuteRejectsUnknownMethod(t *testing.T) {
	w := &fakeWriter{}
	if err := Execute(context.Background(), w, Op{Method: "GET"}); err == nil {
		t.Fatal("expected an error for an unknown method")
	}
	if len(w.calls) != 0 {
		t.Fatalf("unknown method must not reach the router, got %+v", w.calls)
	}
}
